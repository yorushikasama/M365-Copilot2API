package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"m365-copilot2api/internal/chathub"
)

func (s *Server) chatStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	var body chatBody
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "bad json")
		return
	}
	text := strings.TrimSpace(firstNonEmpty(body.Message, body.Prompt))
	if text == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "message required")
		return
	}
	// Only a client-supplied accountId pins the stream to one account; an
	// account restored from sessionKey is a routing hint and may fail over.
	// X-M365-Allow-Failover:true lifts the pin's veto over failover.
	clientPinnedAccount := body.AccountID != "" && !allowFailoverRequested(r)
	if body.SessionKey != "" {
		if v, ok := s.sessions.get(body.SessionKey); ok {
			body.AccountID = firstNonEmpty(body.AccountID, v.AccountID)
			body.ConversationID = firstNonEmpty(body.ConversationID, v.ConversationID)
			body.SessionID = firstNonEmpty(body.SessionID, v.SessionID)
		}
	}
	acc, err := s.resolveAccount(body.AccountID)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	// Cooldown gate: answer 429 locally for an account still inside its quota
	// cooldown instead of forwarding upstream; with X-M365-Allow-Failover the
	// gate fails over to a healthy account instead (see handleCooldownGate in
	// throttle_guard.go).
	if !s.handleCooldownGate(r, w, &acc) {
		return
	}
	if acc.OID == "" || acc.TID == "" {
		if o, t := extractOIDTID(acc.AccessToken); o != "" {
			acc.OID, acc.TID = o, t
		}
	}
	if acc.OID == "" || acc.TID == "" {
		writeOpenAIError(w, http.StatusBadRequest, "account_error", "account missing oid/tid")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.settings.get().ChatTimeoutSeconds)*time.Second)
	defer cancel()
	streamSettings := s.settings.get()
	streamReq := chathub.Request{
		Text: text, Tone: body.Tone, ConversationID: body.ConversationID, SessionID: body.SessionID, Attachments: body.Attachments,
		LicenseType: streamSettings.LicenseType, Scenario: streamSettings.Scenario,
		ConversationSignature: body.ConversationSignature, PreviousMessages: body.PreviousMessages, ConnectedFederatedIDs: body.ConnectedFederatedIDs,
		FeatureFlags: s.featureFlags(),
	}
	res, err := s.chatWithAccount(ctx, acc.ID, chathub.Account{AccessToken: acc.AccessToken, OID: acc.OID, TID: acc.TID}, streamReq)
	triedAccountIDs := map[string]bool{acc.ID: true}
	for err != nil && !clientPinnedAccount && canFailoverChatTurn(ctx, err) && r.Context().Err() == nil {
		next, nerr := s.nextHealthyAccountExcluding(triedAccountIDs)
		if nerr != nil {
			break
		}
		triedAccountIDs[next.ID] = true
		failoverReq := streamReq
		failoverReq.ConversationID = ""
		failoverReq.SessionID = ""
		failoverReq.ConversationSignature = ""
		ctx2, cancel2 := context.WithTimeout(r.Context(), time.Duration(s.settings.get().ChatTimeoutSeconds)*time.Second)
		res2, err2 := s.chatWithAccount(ctx2, next.ID, chathub.Account{AccessToken: next.AccessToken, OID: next.OID, TID: next.TID}, failoverReq)
		cancel2()
		res, err, acc = res2, err2, next
	}
	if err != nil {
		if errors.Is(err, chathub.ErrImageLimit) && s.accountPool != nil {
			s.accountPool.MarkImageLimited(acc.ID)
		}
		writeUpstreamError(w, err)
		return
	}
	if body.SessionKey != "" {
		s.sessions.upsert(conversation{ID: body.SessionKey, AccountID: acc.ID, ConversationID: res.ConversationID, SessionID: res.SessionID, Title: text})
	}
	res.Text = sanitizePublicAssistantText(res.Text)
	res.Text, _ = chathub.StripCitationMarkers(res.Text, res.References)
	res.Reasoning = sanitizePublicReasoningText(res.Reasoning)

	if res.Throttling != nil {
		if b, err := json.Marshal(res.Throttling); err == nil {
			w.Header().Set("X-M365-Throttling", string(b))
		}
	}
	if len(res.Scores) > 0 {
		if b, err := json.Marshal(res.Scores); err == nil {
			w.Header().Set("X-M365-Scores", string(b))
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "stream unsupported")
		return
	}
	sw := newSSEWriter(w, flusher)
	ka := startSSEKeepalive(sw, ctx)
	defer ka.stop()
	// writeSSE writes to w directly and would bypass the sseWriter mutex, racing
	// the keepalive goroutine (net/http writers are not goroutine-safe). Route
	// every frame through sw instead so all writers serialize on one lock.
	writeEvent := func(name string, value any) error {
		if err := r.Context().Err(); err != nil {
			return err
		}
		b, _ := json.Marshal(value)
		return sw.raw(fmt.Sprintf("event: %s\ndata: %s\n\n", name, b))
	}
	for i, event := range res.NormalizedEvents() {
		payload := map[string]any{
			"index":          i,
			"type":           "chathub.event",
			"event":          event,
			"conversationId": res.ConversationID,
			"sessionId":      res.SessionID,
			"requestId":      res.RequestID,
		}
		if err := writeEvent("event", payload); err != nil {
			return
		}
	}
	for i, event := range chathub.SemanticEvents(res.Events) {
		if err := writeEvent("semantic", map[string]any{"index": i, "type": "m365.semantic", "event": sanitizedSemanticEvent(event)}); err != nil {
			return
		}
	}
	if err := writeEvent("done", map[string]any{
		"type": "done", "text": res.Text,
		"conversationId": res.ConversationID, "sessionId": res.SessionID, "requestId": res.RequestID,
		"throttling": res.Throttling, "suggestedResponses": sanitizedSuggestedResponses(res.SuggestedResponses),
		"offense": res.Offense, "scores": res.Scores, "conversationTransferToken": res.ConversationTransferToken,
		"meteringInformation": res.MeteringInformation, "spokenText": stripInternalCitationMarkers(res.SpokenText),
		"storageMessageId": res.StorageMessageID,
		"timestamps":       res.Timestamps,
	}); err != nil {
		return
	}
	if res.Timestamps.RequestSent != "" {
		_ = sw.raw(": m365-metrics " + mustJSON(res.Timestamps) + "\n\n")
	}
}

func writeSSE(r *http.Request, w http.ResponseWriter, f http.Flusher, name string, value any) error {
	if err := r.Context().Err(); err != nil {
		return err
	}
	b, _ := json.Marshal(value)
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b); err != nil {
		return err
	}
	if f != nil {
		f.Flush()
	}
	return nil
}

type meteringInfoItem struct {
	MeterError string `json:"meterError"`
	HasAccess  bool   `json:"hasAccess"`
}

type throttlingMeteringEntry struct {
	RemainingAllowance int `json:"remainingAllowance"`
}

func ParseMetering(accountID string, items json.RawMessage) (meterError string, hasAccess bool) {
	hasAccess = true
	if len(items) == 0 {
		return "", hasAccess
	}
	var parsed []meteringInfoItem
	if json.Unmarshal(items, &parsed) != nil {
		return "", hasAccess
	}
	for _, mi := range parsed {
		if !mi.HasAccess {
			hasAccess = false
			if meterError == "" {
				meterError = mi.MeterError
			}
		}
	}
	if meterError != "" {
		log.Printf("[metering] account=%s meterError=%q hasAccess=%v", accountID, meterError, hasAccess)
	}
	return meterError, hasAccess
}

func remainingAllowances(throttling any) map[string]int {
	remaining := map[string]int{}
	if throttling == nil {
		return remaining
	}
	b, err := json.Marshal(throttling)
	if err != nil {
		return remaining
	}
	var thr struct {
		Metering map[string]throttlingMeteringEntry `json:"metering"`
	}
	if json.Unmarshal(b, &thr) != nil {
		return remaining
	}
	for k, v := range thr.Metering {
		remaining[k] = v.RemainingAllowance
	}
	return remaining
}

func applyMeteringCooldown(pool *accountHealth, accountID string, meterError string) {
	if pool == nil || accountID == "" || meterError == "" {
		return
	}
	switch meterError {
	case "ImageGenInsufficientTokensThrottled":
		pool.MarkImageGenTokensThrottled(accountID)
		log.Printf("[metering] account=%s imageGenCooldownUntil=next_midnight_utc", accountID)
	case "ImageGenSystemCapacityThrottled":
		pool.MarkImageGenSystemThrottled(accountID)
		log.Printf("[metering] account=%s imageGenSystemCooldown=30m", accountID)
	}
}

// sanitizedSemanticEvent strips the upstream citation sentinels from the
// projected fields of a semantic event. Text/HiddenText/Queries are the
// presentation view an API consumer renders, so they carry the same obligation
// as assistant content; the embedded Raw event is deliberately left verbatim,
// because it is the debug passthrough of exactly what the upstream sent.
func sanitizedSemanticEvent(e chathub.SemanticEvent) chathub.SemanticEvent {
	e.Text = stripSemanticFieldMarkers(e.Text)
	e.HiddenText = stripSemanticFieldMarkers(e.HiddenText)
	if len(e.Queries) > 0 {
		queries := make([]string, len(e.Queries))
		for i, q := range e.Queries {
			queries[i] = stripSemanticFieldMarkers(q)
		}
		e.Queries = queries
	}
	return e
}

// stripSemanticFieldMarkers removes complete citation spans, then drops a
// trailing span that upstream cut off mid-marker.
//
// stripInternalCitationMarkers leaves an unterminated span alone on purpose: in
// a streaming fragment the closing half arrives in a later chunk, so cutting at
// the opener would corrupt the join. A semantic event is not a fragment — each
// one is a complete snapshot of the answer so far — so an opener with no closer
// means upstream truncated the snapshot inside the marker, and there is no
// continuation coming. Everything from that opener onward is marker debris:
// leaving it produced a bare U+E200 at the end of 6 progress snapshots, and
// removing only the sentinel runes would be worse, surfacing a half-written
// "citecall_4eb" payload as visible text.
func stripSemanticFieldMarkers(text string) string {
	text = stripInternalCitationMarkers(text)
	i := strings.IndexRune(text, citationMarkerOpen)
	if i < 0 {
		return text
	}
	return text[:i]
}

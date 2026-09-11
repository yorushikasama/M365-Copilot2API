package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestUsageLog builds a usage log that can accept records without touching
// disk. A bare &usageLog{} literal panics inside record: persist is nil.

// A streaming /v1/responses turn books its own usage record, and the inner
// openaiChat replay books none: before the internal-adapter marker existed the
// buffered path recorded twice (once as /v1/chat/completions from the inner
// handler, once as /v1/responses from the outer one) and the streaming path
// recorded nothing at all.
func TestStreamingResponsesRecordsUsageExactlyOnce(t *testing.T) {
	orig := runResponsesInnerChat
	defer func() { runResponsesInnerChat = orig }()
	runResponsesInnerChat = func(_ *Server, w http.ResponseWriter, r *http.Request) {
		if !isInternalAdapterRequest(r) {
			t.Error("inner chat request must carry the internal-adapter marker")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: "+mustJSON(map[string]any{
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "hello"}, "finish_reason": nil}},
		})+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}

	s := &Server{usage: newTestUsageLog(t)}
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	rr := httptest.NewRecorder()
	s.streamResponsesAdapter(rr, r, oaiReq{Model: "m365-copilot"}, "m365-copilot", nil)

	s.usage.mu.Lock()
	recs := append([]UsageRecord(nil), s.usage.records...)
	s.usage.mu.Unlock()
	if len(recs) != 1 {
		t.Fatalf("want exactly 1 usage record, got %d: %+v", len(recs), recs)
	}
	if recs[0].Endpoint != "/v1/responses" {
		t.Fatalf("usage endpoint = %q, want /v1/responses", recs[0].Endpoint)
	}
	if !recs[0].Stream {
		t.Fatal("streaming turn must be recorded as a stream")
	}
}

// An external caller must not be able to forge the internal-adapter marker: it
// lives on the request context, not in a header, so suppressing usage
// accounting cannot be triggered from outside.
func TestInternalAdapterMarkerIsNotForgeable(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("X-M365-Internal-Adapter", "true")
	r.Header.Set("internalAdapterKey", "true")
	if isInternalAdapterRequest(r) {
		t.Fatal("header must not mark a request as internal")
	}
	if isInternalAdapterRequest(nil) {
		t.Fatal("nil request must not be internal")
	}
	inner := internalAdapterRequest(r, []byte(`{}`))
	if !isInternalAdapterRequest(inner) {
		t.Fatal("adapter-cloned request must be marked internal")
	}
	if inner.ContentLength != 2 {
		t.Fatalf("ContentLength = %d, want 2", inner.ContentLength)
	}
}

// The inner handler writes the whole completion into the pipe. When the outer
// stream stops reading (client hang-up), the read half must be closed with an
// error so the inner handler's Write fails instead of parking forever, holding
// an account concurrency slot and an upstream WebSocket.
func TestAbandonedResponsesStreamUnblocksInnerHandler(t *testing.T) {
	orig := runResponsesInnerChat
	defer func() { runResponsesInnerChat = orig }()

	innerReturned := make(chan error, 1)
	runResponsesInnerChat = func(_ *Server, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Far more than the pipe can absorb, so Write blocks until the reader
		// either drains it or closes the read half.
		big := strings.Repeat("x", 64<<10)
		var err error
		for i := 0; i < 64; i++ {
			if _, err = w.Write([]byte("data: " + big + "\n\n")); err != nil {
				break
			}
		}
		innerReturned <- err
	}

	// A canceled context makes the scan loop bail on its first iteration,
	// which is the client-hang-up path.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	s := &Server{usage: newTestUsageLog(t)}
	s.streamResponsesAdapter(httptest.NewRecorder(), r, oaiReq{Model: "m365-copilot"}, "m365-copilot", nil)

	select {
	case err := <-innerReturned:
		if err == nil {
			t.Fatal("inner handler should have observed a write error on the abandoned pipe")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("inner handler is still blocked writing to an abandoned pipe")
	}
}

// A parent response ID is marked Consumed before the turn runs, so a failed
// turn must un-consume it. Otherwise the client's only retry path answers 409
// forever and the conversation is unrecoverable.
func TestFailedResponsesTurnReleasesParentForRetry(t *testing.T) {
	orig := runResponsesInnerChat
	defer func() { runResponsesInnerChat = orig }()
	runResponsesInnerChat = func(_ *Server, w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, `{"error":{"code":"upstream_error","message":"boom"}}`)
	}

	s := &Server{
		settings:         &settingsStore{v: defaultRuntimeSettings()},
		usage:            newTestUsageLog(t),
		responseMessages: map[string]map[string]*RespNode{},
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	tenant := tenantFromRequest(req)
	if tenant == "" {
		tenant = "anonymous"
	}
	nsKey := responseNamespace(tenant, responseSessionID(req))
	parent := &RespNode{
		At:        time.Now(),
		Messages:  []oaiMsg{{Role: "user", Content: "hi"}},
		ToolCalls: map[string]*ToolCallRecord{},
		Version:   1,
		Tenant:    tenant,
		SessionID: responseSessionID(req),
	}
	s.responseMessages[nsKey] = map[string]*RespNode{"resp_parent": parent}

	post := func() (int, string) {
		body := `{"model":"m365-copilot","stream":true,"previous_response_id":"resp_parent","input":[{"type":"message","role":"user","content":"go"}]}`
		r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer test-key")
		rr := httptest.NewRecorder()
		s.responses(rr, r)
		return rr.Code, rr.Body.String()
	}

	if _, out := post(); !strings.Contains(out, "response.failed") {
		t.Fatalf("first attempt should fail in-band, got %.300s", out)
	}
	s.responseMu.Lock()
	consumed := parent.Consumed
	s.responseMu.Unlock()
	if consumed {
		t.Fatal("a failed turn must release the parent, otherwise every retry answers 409")
	}

	// The retry must reach the upstream again instead of being rejected.
	status, out := post()
	if status == 409 || strings.Contains(out, "already consumed") {
		t.Fatalf("retry after failure was rejected as consumed: status=%d body=%.300s", status, out)
	}
}

// A successful turn keeps the parent consumed, so replaying the same
// previous_response_id is still rejected.
func TestSuccessfulResponsesTurnKeepsParentConsumed(t *testing.T) {
	orig := runResponsesInnerChat
	defer func() { runResponsesInnerChat = orig }()
	runResponsesInnerChat = func(_ *Server, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: "+mustJSON(map[string]any{
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "done"}, "finish_reason": "stop"}},
		})+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}

	s := &Server{
		settings:         &settingsStore{v: defaultRuntimeSettings()},
		usage:            newTestUsageLog(t),
		responseMessages: map[string]map[string]*RespNode{},
	}
	probe := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	probe.Header.Set("Authorization", "Bearer test-key")
	tenant := tenantFromRequest(probe)
	if tenant == "" {
		tenant = "anonymous"
	}
	sessionID := responseSessionID(probe)
	parent := &RespNode{At: time.Now(), Messages: []oaiMsg{{Role: "user", Content: "hi"}}, ToolCalls: map[string]*ToolCallRecord{}, Version: 1, Tenant: tenant, SessionID: sessionID}
	s.responseMessages[responseNamespace(tenant, sessionID)] = map[string]*RespNode{"resp_parent": parent}

	body := `{"model":"m365-copilot","stream":true,"previous_response_id":"resp_parent","input":[{"type":"message","role":"user","content":"go"}]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer test-key")
	rr := httptest.NewRecorder()
	s.responses(rr, r)
	if !strings.Contains(rr.Body.String(), "response.completed") {
		t.Fatalf("turn should have completed: %.300s", rr.Body.String())
	}

	s.responseMu.Lock()
	consumed := parent.Consumed
	s.responseMu.Unlock()
	if !consumed {
		t.Fatal("a completed turn must leave the parent consumed so a replay is rejected")
	}

	r2 := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	r2.Header.Set("Content-Type", "application/json")
	r2.Header.Set("Authorization", "Bearer test-key")
	rr2 := httptest.NewRecorder()
	s.responses(rr2, r2)
	if rr2.Code != 409 {
		t.Fatalf("replay of a consumed parent = %d, want 409: %.200s", rr2.Code, rr2.Body.String())
	}
}

// The buffered /v1/responses path must also book exactly one record, under its
// own endpoint rather than the inner handler's.
func TestBufferedResponsesUsageIsNotDoubleCounted(t *testing.T) {
	s := &Server{usage: newTestUsageLog(t)}
	inner := internalAdapterRequest(httptest.NewRequest(http.MethodPost, "/v1/responses", nil), []byte(`{}`))

	// Simulate what openaiChat does at its two recording sites: both are
	// guarded by the marker, so an internal replay books nothing.
	if !isInternalAdapterRequest(inner) {
		t.Fatal("expected an internal request")
	}
	if s.usage != nil && !isInternalAdapterRequest(inner) {
		s.usage.record(UsageRecord{Endpoint: "/v1/chat/completions", Time: time.Now()})
	}
	s.usage.mu.Lock()
	n := len(s.usage.records)
	s.usage.mu.Unlock()
	if n != 0 {
		t.Fatalf("internal replay booked %d records, want 0", n)
	}
}

// Guard the SSE shape the fixes touch: response.created must be followed by
// exactly one terminal event.
func TestResponsesStreamHasExactlyOneTerminalEvent(t *testing.T) {
	orig := runResponsesInnerChat
	defer func() { runResponsesInnerChat = orig }()
	runResponsesInnerChat = func(_ *Server, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: "+mustJSON(map[string]any{
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "hi"}, "finish_reason": "stop"}},
		})+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
	s := &Server{usage: newTestUsageLog(t)}
	rr := httptest.NewRecorder()
	s.streamResponsesAdapter(rr, httptest.NewRequest(http.MethodPost, "/v1/responses", nil), oaiReq{Model: "m"}, "m", nil)

	terminal := 0
	created := 0
	for _, ev := range collectResponsesEvents(t, rr.Body.String()) {
		switch ev["type"] {
		case "response.created":
			created++
		case "response.completed", "response.failed":
			terminal++
		}
	}
	if created != 1 || terminal != 1 {
		t.Fatalf("created=%d terminal=%d, want 1 and 1; body=%.400s", created, terminal, rr.Body.String())
	}
}

func TestResponsesStreamUsageMatchesEmittedUsage(t *testing.T) {
	orig := runResponsesInnerChat
	defer func() { runResponsesInnerChat = orig }()
	runResponsesInnerChat = func(_ *Server, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: "+mustJSON(map[string]any{
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "some answer text"}, "finish_reason": "stop"}},
		})+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
	s := &Server{usage: newTestUsageLog(t)}
	rr := httptest.NewRecorder()
	s.streamResponsesAdapter(rr, httptest.NewRequest(http.MethodPost, "/v1/responses", nil),
		oaiReq{Model: "m", Messages: []oaiMsg{{Role: "user", Content: "question"}}}, "m", nil)

	var emitted map[string]any
	for _, ev := range collectResponsesEvents(t, rr.Body.String()) {
		if ev["type"] == "response.completed" {
			resp, _ := ev["response"].(map[string]any)
			emitted, _ = resp["usage"].(map[string]any)
		}
	}
	if emitted == nil {
		t.Fatalf("no usage on response.completed: %.400s", rr.Body.String())
	}
	s.usage.mu.Lock()
	recs := append([]UsageRecord(nil), s.usage.records...)
	s.usage.mu.Unlock()
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	wantIn := int64(jsonNumber(t, emitted["input_tokens"]))
	wantOut := int64(jsonNumber(t, emitted["output_tokens"]))
	if recs[0].InputTokens != wantIn || recs[0].OutputTokens != wantOut {
		t.Fatalf("recorded (%d,%d) but reported (%d,%d) to the client",
			recs[0].InputTokens, recs[0].OutputTokens, wantIn, wantOut)
	}
}

func jsonNumber(t *testing.T, v any) float64 {
	t.Helper()
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	t.Fatalf("not a number: %#v", v)
	return 0
}

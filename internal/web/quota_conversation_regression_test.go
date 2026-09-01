package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"m365-copilot2api/internal/auth"
	"m365-copilot2api/internal/chathub"
)

func TestM365ConversationListFiltersMissingHistory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_SESSION_CACHE", filepath.Join(dir, "sessions.json"))
	t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(dir, "conversations.json"))
	store, err := auth.OpenStore(filepath.Join(dir, "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{tokens: store, sessionResolver: openSessionResolver()}
	old := m365CloudClient
	m365CloudClient = nil
	defer func() { m365CloudClient = old }()

	body := &oaiReq{Messages: []oaiMsg{{Role: "user", Content: "local history"}}}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	s.sessionResolver.Bind("", "local-with-history", "account-a", body, "", req)
	// This binding is metadata-only and must not appear in the admin list.
	s.sessionResolver.Bind("", "local-empty", "account-a", &oaiReq{}, "", req)

	rr := httptest.NewRecorder()
	s.handleM365Conversations(rr, httptest.NewRequest(http.MethodGet, "/api/m365/conversations", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var response struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Data) != 1 || response.Data[0]["conversationId"] != "local-with-history" {
		t.Fatalf("filtered list=%s", rr.Body.String())
	}
}

func TestAllowanceEstimateDecrementsAndDoesNotIncrease(t *testing.T) {
	h := newAccountHealth()
	const id = "account-a"
	h.UpdateMetering(id, "", true, map[string]int{"LLMOnly": 5})
	h.RecordAllowanceConsumption(id, "LLMOnly")
	est, used, _ := h.GetAllowanceSnapshot(id)
	if est["LLMOnly"] != 4 || used["LLMOnly"] != 1 {
		t.Fatalf("estimate=%v consumed=%v", est, used)
	}
	// A stale upstream snapshot must not restore the consumed unit.
	h.UpdateMetering(id, "", true, map[string]int{"LLMOnly": 5})
	est, _, _ = h.GetAllowanceSnapshot(id)
	if est["LLMOnly"] != 4 {
		t.Fatalf("stale snapshot restored quota: %v", est)
	}
	// A lower upstream observation is authoritative and rebases downward.
	h.UpdateMetering(id, "", true, map[string]int{"LLMOnly": 3})
	est, _, _ = h.GetAllowanceSnapshot(id)
	if est["LLMOnly"] != 3 {
		t.Fatalf("lower snapshot not applied: %v", est)
	}
	// Missing metering must preserve the last valid observation.
	h.UpdateMetering(id, "", true, nil)
	est, _, _ = h.GetAllowanceSnapshot(id)
	if est["LLMOnly"] != 3 {
		t.Fatalf("empty snapshot erased estimate: %v", est)
	}
}

func TestAllowanceEstimateIsCapabilityScoped(t *testing.T) {
	h := newAccountHealth()
	const id = "account-a"
	h.UpdateMetering(id, "", true, map[string]int{"LLMOnly": 5, "ImageGeneration": 3})
	h.RecordAllowanceConsumption(id, "ImageGeneration")
	est, _, _ := h.GetAllowanceSnapshot(id)
	if est["ImageGeneration"] != 2 || est["LLMOnly"] != 5 {
		t.Fatalf("capability estimate=%v", est)
	}
}

func TestAllowanceEstimateHasTimestamp(t *testing.T) {
	h := newAccountHealth()
	before := time.Now()
	h.UpdateMetering("account-a", "", true, map[string]int{"LLMOnly": 1})
	_, _, updated := h.GetAllowanceSnapshot("account-a")
	if updated.Before(before) {
		t.Fatalf("updatedAt=%v before %v", updated, before)
	}
}

func TestMeteringErrorStillUpdatesAccountQuotaState(t *testing.T) {
	h := newAccountHealth()
	s := &Server{accountPool: h, settings: &settingsStore{v: defaultRuntimeSettings()}}
	metering := []any{map[string]any{"meterError": "ImageGenInsufficientTokensThrottled", "hasAccess": false}}
	err := &chathub.MeteringError{
		Cause:      chathub.ErrImageLimit,
		Throttling: map[string]any{"metering": map[string]any{"ImageGeneration": map[string]any{"remainingAllowance": 7}}},
		Metering:   metering,
	}
	s.recordAccountChatResult("account-a", chathub.Result{}, err)
	meterError, hasAccess, remaining := h.GetMetering("account-a")
	if meterError != "ImageGenInsufficientTokensThrottled" || hasAccess || remaining["ImageGeneration"] != 7 {
		t.Fatalf("metering error state=%q access=%v remaining=%v", meterError, hasAccess, remaining)
	}
	est, _, _ := h.GetAllowanceSnapshot("account-a")
	if est["ImageGeneration"] != 7 {
		t.Fatalf("error response did not seed allowance estimate: %v", est)
	}
}

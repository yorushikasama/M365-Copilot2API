package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Reproduction harness for the "instant 200 with a tiny body" incident on
// /v1/responses (2026-09-08): Codex-style requests with hundreds of messages
// came back in <15ms with 200 and ~436 bytes while openaiChat logged nothing
// past body_parsed. Each scenario below trips one of the early-exit branches
// between body_parsed and prompt_flattened and records what the OUTER
// /v1/responses response looks like.

func responsesReproServer() *Server {
	cfg := defaultRuntimeSettings()
	return &Server{settings: &settingsStore{v: cfg}}
}

func postResponses(t *testing.T, s *Server, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-key")
	rr := httptest.NewRecorder()
	s.responses(rr, req)
	return rr.Code, rr.Body.String()
}

// Scenario 1: messages whose tool-call protocol is violated (assistant tool
// call not followed by its tool result). Since 2026-09-09 these are HEALED
// (placeholder results synthesized) instead of rejected with 400 — the bare
// harness Server cannot run the full downstream pipeline, so any deeper panic
// here actually proves the request progressed past validation.
func TestResponsesReproToolProtocolViolation(t *testing.T) {
	defer func() { _ = recover() }()
	s := responsesReproServer()
	input := []any{
		map[string]any{"type": "message", "role": "user", "content": "hi"},
		map[string]any{"type": "message", "role": "assistant", "content": "calling tool"},
		map[string]any{"type": "function_call", "call_id": "call_1", "name": "shell", "arguments": "{}"},
		// NOTE: no function_call_output for call_1 — protocol violation.
		map[string]any{"type": "message", "role": "user", "content": "continue"},
	}
	payload := `{"model":"gpt-5.6-sol","stream":false,"input":` + mustJSON(input) + `}`
	status, body := postResponses(t, s, payload)
	if status == 400 && strings.Contains(body, "tool_protocol_error") {
		t.Fatalf("protocol violation must be healed, not rejected: %s", body)
	}
	t.Logf("scenario1 status=%d bytes=%d body=%.300s", status, len(body), body)
}

// Scenario 2: too many completed tool rounds in the current turn → 409
// tool_round_limit.
func TestResponsesReproRoundLimit(t *testing.T) {
	s := responsesReproServer()
	input := []any{map[string]any{"type": "message", "role": "user", "content": "go"}}
	for i := 0; i < 60; i++ {
		input = append(input,
			map[string]any{"type": "function_call", "call_id": "call_x", "name": "shell", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "call_x", "output": "ok"},
		)
	}
	payload := `{"model":"gpt-5.6-sol","stream":false,"input":` + mustJSON(input) + `}`
	status, body := postResponses(t, s, payload)
	t.Logf("scenario2 status=%d bytes=%d body=%.300s", status, len(body), body)
}

// Scenario 3: valid messages but a prompt that exceeds the context budget
// error path (slidingWindow budgetErr).
func TestResponsesReproContextBudget(t *testing.T) {
	s := responsesReproServer()
	huge := strings.Repeat("x", 4_000_000)
	input := []any{map[string]any{"type": "message", "role": "user", "content": huge}}
	payload := `{"model":"gpt-5.6-sol","stream":false,"input":` + mustJSON(input) + `}`
	status, body := postResponses(t, s, payload)
	t.Logf("scenario3 status=%d bytes=%d body=%.300s", status, len(body), body)
}

// Scenario 4: a STREAMING request whose inner chat turn is rejected must
// surface the real rejection reason in the response.failed event. Regression
// for the 2026-09-08 incident where Codex received a generic
// "inner chat request failed" (or worse, nothing diagnosable) and the actual
// guard verdict was invisible in the gateway logs.
func TestResponsesReproStreamSurfacesInnerError(t *testing.T) {
	defer func() { _ = recover() }() // bare harness cannot run the full healed pipeline
	s := responsesReproServer()
	input := []any{
		map[string]any{"type": "message", "role": "user", "content": "hi"},
		map[string]any{"type": "function_call", "call_id": "call_1", "name": "shell", "arguments": "{}"},
		// NOTE: no function_call_output for call_1 — protocol violation.
		map[string]any{"type": "message", "role": "user", "content": "continue"},
	}
	payload := `{"model":"gpt-5.6-sol","stream":true,"input":` + mustJSON(input) + `}`
	status, body := postResponses(t, s, payload)
	t.Logf("scenario4 status=%d bytes=%d body=%.500s", status, len(body), body)
	if strings.Contains(body, "tool_protocol_error") || strings.Contains(body, "missing tool result") {
		t.Fatalf("protocol violation must be healed, not rejected: %.400s", body)
	}
	t.Logf("scenario4 status=%d bytes=%d body=%.500s", status, len(body), body)
}

// The 2026-09-09 incident: a long ZCode history on /v1/responses hit
// "tool results missing before assistant message at index 197" — a hard 400
// that the client surfaced as an instant empty_model_response. The gateway
// must heal these histories instead of rejecting the whole session.
func TestRepairToolConversationHealsBrokenHistories(t *testing.T) {
	// Case 1: assistant message arrives while a call is unresolved → placeholder inserted.
	msgs := []oaiMsg{
		{Role: "user", Content: "go"},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "call_a", "type": "function", "function": map[string]any{"name": "Bash", "arguments": "{}"}}}},
		{Role: "assistant", Content: "thinking about it"},
		{Role: "tool", ToolCallID: "call_a", Content: "ok"},
	}
	out, err := repairToolConversation("t1", msgs)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 4 || out[2].Role != "tool" || out[2].ToolCallID != "call_a" {
		t.Fatalf("placeholder result not inserted: %+v", out)
	}

	// Case 2: orphan tool result (call dropped by client compaction) → dropped.
	msgs = []oaiMsg{
		{Role: "user", Content: "go"},
		{Role: "tool", ToolCallID: "call_gone", Content: "orphan"},
		{Role: "assistant", Content: "done"},
	}
	out, err = repairToolConversation("t2", msgs)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[1].Role != "assistant" {
		t.Fatalf("orphan result not dropped: %+v", out)
	}

	// Case 3: history ends with unresolved call → placeholder appended.
	msgs = []oaiMsg{
		{Role: "user", Content: "go"},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "call_b", "type": "function", "function": map[string]any{"name": "Read", "arguments": "{}"}}}},
	}
	out, err = repairToolConversation("t3", msgs)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 || out[2].Role != "tool" || out[2].ToolCallID != "call_b" {
		t.Fatalf("trailing unresolved call not closed: %+v", out)
	}

	// Case 4: genuinely malformed (call without id) still errors.
	msgs = []oaiMsg{
		{Role: "assistant", ToolCalls: []map[string]any{{"type": "function", "function": map[string]any{"name": "Read", "arguments": "{}"}}}},
	}
	if _, err := repairToolConversation("t4", msgs); err == nil {
		t.Fatal("call without id must still be rejected")
	}
}

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
// call not followed by its tool result) → validateToolConversation 400.
func TestResponsesReproToolProtocolViolation(t *testing.T) {
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
	if !strings.Contains(body, "response.failed") {
		t.Fatal("streaming inner rejection must end with response.failed")
	}
	if !strings.Contains(body, "missing tool result") {
		t.Fatalf("response.failed must carry the real inner error, got: %.400s", body)
	}
	if strings.Contains(body, "inner chat request failed") {
		t.Fatal("generic message must be replaced by the real inner error")
	}
}

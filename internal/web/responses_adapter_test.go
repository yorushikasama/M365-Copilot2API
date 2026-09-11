package web

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// responsesAdapterStubChat replays the exact chunk sequence writeToolResponse
// produces in stream mode for one function tool call.
func responsesAdapterStubChat(calls []map[string]any, text string) func(*Server, http.ResponseWriter, *http.Request) {
	return func(_ *Server, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		base := func(delta map[string]any, finish any) string {
			return "data: " + mustJSON(map[string]any{"id": "c1", "object": "chat.completion.chunk", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}) + "\n\n"
		}
		fmt.Fprint(w, base(map[string]any{"role": "assistant", "content": nil}, nil))
		if text != "" {
			fmt.Fprint(w, base(map[string]any{"content": text}, nil))
		}
		for _, tc := range calls {
			fmt.Fprint(w, base(map[string]any{"tool_calls": []any{map[string]any{"index": tc["index"], "id": tc["id"], "type": "function", "function": map[string]any{"name": tc["name"], "arguments": ""}}}}, nil))
			fmt.Fprint(w, base(map[string]any{"tool_calls": []any{map[string]any{"index": tc["index"], "function": map[string]any{"arguments": tc["arguments"]}}}}, "tool_calls"))
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

func collectResponsesEvents(t *testing.T, body string) []map[string]any {
	t.Helper()
	var events []map[string]any
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m) == nil && m["type"] != nil {
			events = append(events, m)
		}
	}
	return events
}

// Regression for the 2026-09-09 ZCode failure: the added event used to carry
// name="" and call_id="" because it was emitted before the declaration chunk
// was parsed. ZCode's AI-SDK adapter aborts the turn with "tool name is
// empty" (classified invalid_request) on such an event.
func TestResponsesAdapterToolCallAddedCarriesIdentity(t *testing.T) {
	orig := runResponsesInnerChat
	defer func() { runResponsesInnerChat = orig }()
	runResponsesInnerChat = responsesAdapterStubChat([]map[string]any{{
		"index": 0.0, "id": "call_abc", "name": "Agent",
		"arguments": `{"prompt":"do the work"}`,
	}}, "")

	s := &Server{}
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	rr := httptest.NewRecorder()
	s.streamResponsesAdapter(rr, r, oaiReq{Model: "gpt-5.6-sol"}, "gpt-5.6-sol", nil)

	events := collectResponsesEvents(t, rr.Body.String())
	var added map[string]any
	for _, ev := range events {
		if ev["type"] == "response.output_item.added" {
			added = ev
			break
		}
	}
	if added == nil {
		t.Fatalf("no output_item.added event; body=%.400s", rr.Body.String())
	}
	item, _ := added["item"].(map[string]any)
	if item == nil || item["type"] != "function_call" {
		t.Fatalf("added item is not a function_call: %#v", added["item"])
	}
	if name, _ := item["name"].(string); name != "Agent" {
		t.Fatalf("added item name must be populated, got %q", name)
	}
	if callID, _ := item["call_id"].(string); callID != "call_abc" {
		t.Fatalf("added item call_id must be populated, got %q", callID)
	}
	var done map[string]any
	for _, ev := range events {
		if ev["type"] == "response.output_item.done" {
			done = ev
		}
	}
	if done == nil {
		t.Fatal("no output_item.done event")
	}
	ditem, _ := done["item"].(map[string]any)
	if args, _ := ditem["arguments"].(string); args != `{"prompt":"do the work"}` {
		t.Fatalf("done item arguments wrong: %q", args)
	}
}

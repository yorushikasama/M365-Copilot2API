package web

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

func jsonMarshalForTest(v any) ([]byte, error) { return json.Marshal(v) }

func TestCanonicalToolArgumentsDeduplicateEquivalentJSON(t *testing.T) {
	ledger := agentLedger{Completed: []toolEvidence{{
		Name:      "workspace_write_file",
		Arguments: `{"path":"main.go","content":"x"}`,
	}}}
	if !ledger.hasCompleted("workspace_write_file", ` { "content":"x", "path":"main.go" } `) {
		t.Fatal("equivalent JSON arguments were not deduplicated")
	}
}

func TestFilterCompletedCallsKeepsNewArguments(t *testing.T) {
	ledger := agentLedger{Completed: []toolEvidence{{
		Name:      "workspace_write_file",
		Arguments: `{"path":"main.go","content":"old"}`,
	}}}
	calls := []detectedToolCall{
		{Name: "workspace_write_file", Arguments: []byte(`{"path":"main.go","content":"old"}`)},
		{Name: "workspace_write_file", Arguments: []byte(`{"path":"main.go","content":"new"}`)},
	}
	got := filterCompletedCalls(calls, ledger)
	if len(got) != 1 || string(got[0].Arguments) != `{"path":"main.go","content":"new"}` {
		t.Fatalf("unexpected filtered calls: %#v", got)
	}
}

func TestRouterContextStaysCompact(t *testing.T) {
	ledger := agentLedger{Completed: []toolEvidence{{
		ID: "call_1", Name: "workspace_write_file", Arguments: `{"path":"main.go"}`, Result: "written successfully",
	}}}
	ctx := ledger.RouterContext()
	if len(ctx) > 2000 {
		t.Fatalf("router context unexpectedly large: %d bytes", len(ctx))
	}
	if len(ctx) == 0 {
		t.Fatal("router context is empty")
	}
}

// A second Read of the same file is legitimate: the file may have changed.
// Dropping it as "already completed" emptied the router decision, and the turn
// fell through to the answer turn where the model produced prose instead of the
// file content (observed 2026-09-10 01:14Z, [router-nocalls] upstream_text=
// "CALL_TOOL: Read(...)" with parsed=true).
func TestFilterCompletedCallsKeepsRepeatableReads(t *testing.T) {
	ledger := agentLedger{Completed: []toolEvidence{
		{Name: "Read", Arguments: `{"file_path":"D:\\a\\b.js"}`, Result: "old content"},
		{Name: "Write", Arguments: `{"file_path":"D:\\a\\b.js","content":"x"}`, Result: "ok"},
	}}
	calls := []detectedToolCall{
		{Name: "Read", Arguments: []byte(`{"file_path":"D:\\a\\b.js"}`)},
		{Name: "Write", Arguments: []byte(`{"file_path":"D:\\a\\b.js","content":"x"}`)},
	}
	got := filterCompletedCalls(calls, ledger)
	if len(got) != 1 || got[0].Name != "Read" {
		t.Fatalf("repeatable Read must survive dedup, mutating Write must not: %#v", got)
	}
}

// When a call IS dropped, the answer turn must be told what already happened —
// including the recorded result — instead of being left to invent an answer.
func TestDuplicateCallNoticeSurfacesRecordedResult(t *testing.T) {
	ledger := agentLedger{Completed: []toolEvidence{{
		Name: "Write", Arguments: `{"path":"main.go","content":"x"}`, Result: "written successfully",
	}}}
	calls := []detectedToolCall{{Name: "Write", Arguments: []byte(`{"path":"main.go","content":"x"}`)}}
	notice := duplicateCallNotice(calls, ledger)
	if !strings.Contains(notice, "written successfully") {
		t.Fatalf("notice must carry the recorded result: %q", notice)
	}
	if !strings.Contains(notice, "Do not repeat this call") {
		t.Fatalf("notice must instruct the model: %q", notice)
	}
	if duplicateCallNotice(nil, ledger) != "" {
		t.Fatal("no dropped calls must produce no notice")
	}
}

// The router uploads the tool catalogue on every tool decision. With 50+ coding
// agent tools the full JSON schemas were the larger half of a 213KB route
// prompt (2026-09-10), so the catalogue is reduced to name/desc/args.
func TestRouterToolCatalogueStaysCompact(t *testing.T) {
	tools := make([]map[string]any, 0, 50)
	for i := 0; i < 50; i++ {
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
			"name":        "tool_" + strconv.Itoa(i),
			"description": strings.Repeat("d", 2000),
			"parameters": map[string]any{
				"type":     "object",
				"required": []any{"a"},
				"properties": map[string]any{
					"a": map[string]any{"type": "string", "description": strings.Repeat("x", 500)},
					"b": map[string]any{"type": "object", "properties": map[string]any{"k1": map[string]any{"type": "string"}}},
				},
			},
		}})
	}
	full, _ := jsonMarshalForTest(tools)
	compact, _ := jsonMarshalForTest(routerToolCatalogue(tools))
	if len(compact) >= len(full) {
		t.Fatalf("compact catalogue=%d must be smaller than full=%d", len(compact), len(full))
	}
	// 50 tools × ~300 chars of description + short arg lines stays in the low
	// tens of KB even with the worst-case descriptions above.
	if len(compact) > 40*1024 {
		t.Fatalf("compact catalogue too large: %d bytes", len(compact))
	}
	compactStr := string(compact)
	if !strings.Contains(compactStr, `"name":"tool_0"`) || !strings.Contains(compactStr, `"a":"string*"`) {
		t.Fatalf("catalogue must keep names and required-argument markers: %.200s", compactStr)
	}
}

// The ledger keeps every completed call for duplicate filtering, but the
// ROUTER PROMPT rendering must stay bounded: 30 completed calls at up to 4KB
// of result each used to produce ~120KB evidence blocks that dominated the
// route prompt even after window slimming.
func TestRouterContextBoundedUnderManyEntries(t *testing.T) {
	var completed []toolEvidence
	for i := 0; i < 30; i++ {
		completed = append(completed, toolEvidence{
			ID:        "call_" + strconv.Itoa(i),
			Name:      "Bash",
			Arguments: `{"command":"echo ` + strconv.Itoa(i) + `"}`,
			Result:    strings.Repeat("x", 4000),
		})
	}
	ledger := agentLedger{Completed: completed}
	ctx := ledger.RouterContext()
	if len(ctx) > 8000 {
		t.Fatalf("router context not bounded: %d bytes", len(ctx))
	}
	if !strings.Contains(ctx, "older") {
		t.Fatal("omitted-entry note missing")
	}
	// Duplicate filtering must still see the FULL ledger, not the bounded view.
	calls := []detectedToolCall{{Name: "Bash", Arguments: []byte(`{"command":"echo 3"}`)}}
	if got := filterCompletedCalls(calls, ledger); len(got) != 0 {
		t.Fatalf("bounded rendering must not weaken dedup: %d calls survived", len(got))
	}
}

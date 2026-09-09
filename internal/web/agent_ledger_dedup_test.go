package web

import (
	"strconv"
	"strings"
	"testing"
)

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

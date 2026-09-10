package web

import (
	"strings"
	"testing"

	"m365-copilot2api/internal/chathub"
)

// Regression for the 2026-09-10 live audit: JSON null content must never turn
// into the literal string "<nil>" on the upstream prompt. OpenAI sends
// content:null on every assistant message that carries tool_calls, so the old
// fmt.Sprint fallback contaminated the history of every multi-round tool call
// (and produced prompt_len=12 == len("[user]\n<nil>") for empty user turns).
func TestNilContentNeverReachesPromptAsLiteral(t *testing.T) {
	if s, _ := parseContent(nil); s != "" {
		t.Fatalf("parseContent(nil) = %q, want empty string", s)
	}

	msgs := []oaiMsg{
		{Role: "user", Content: "read C:\\tmp\\a.txt"},
		{Role: "assistant", Content: nil, ToolCalls: []map[string]any{
			{"id": "call_a1", "type": "function",
				"function": map[string]any{"name": "read_file", "arguments": `{"path":"C:\\tmp\\a.txt"}`}},
		}},
		{Role: "tool", ToolCallID: "call_a1", Content: "hello world from a.txt"},
		{Role: "user", Content: "what did it contain?"},
	}
	got, _ := flattenPromptMessages(msgs, nil)
	if strings.Contains(got, "<nil>") {
		t.Fatalf("literal <nil> leaked into the upstream prompt:\n%s", got)
	}
	if !strings.Contains(got, "[assistant tool_calls]") {
		t.Fatalf("tool_calls block was dropped:\n%s", got)
	}
	if !strings.Contains(got, "hello world from a.txt") {
		t.Fatalf("tool result was dropped:\n%s", got)
	}

	empty, _ := flattenPromptMessages([]oaiMsg{{Role: "user", Content: nil}}, nil)
	if empty != "" {
		t.Fatalf("empty user content should flatten to an empty prompt, got %q", empty)
	}
}

// Upstream account memory must stay off by default; only an explicit operator
// opt-in re-enables it, and a caller-requested one-shot session always wins.
func TestAnswerRequestDisablesUpstreamMemoryByDefault(t *testing.T) {
	body := answerRequestTestBody()

	def := buildAnswerRequest("hi", "magic", body, agentLedger{}, "router",
		runtimeSettings{}, chathub.FeatureFlags{}, chathubLocale{}, false)
	if !def.DisableMemory {
		t.Fatal("upstream account memory must be disabled by default")
	}

	optedIn := buildAnswerRequest("hi", "magic", body, agentLedger{}, "router",
		runtimeSettings{EnableUpstreamMemory: true}, chathub.FeatureFlags{}, chathubLocale{}, false)
	if optedIn.DisableMemory {
		t.Fatal("M365_ENABLE_UPSTREAM_MEMORY=true must re-enable upstream memory")
	}

	oneShot := buildAnswerRequest("hi", "magic", body, agentLedger{}, "router",
		runtimeSettings{EnableUpstreamMemory: true}, chathub.FeatureFlags{}, chathubLocale{}, true)
	if !oneShot.DisableMemory {
		t.Fatal("copilot_temp_session must force memory off even when opted in")
	}
}

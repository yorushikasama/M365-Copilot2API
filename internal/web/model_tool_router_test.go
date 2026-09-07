package web

import (
	"strings"
	"testing"
)

func TestParseModelToolDecisionAutoAndParallel(t *testing.T) {
	calls, ok := parseModelToolDecision(`{"calls":[{"name":"get_weather","arguments":{"city":"Beijing"}},{"name":"get_time","arguments":{"city":"Beijing"}}]}`, testTools(), "auto")
	if !ok || len(calls) != 2 {
		t.Fatalf("calls=%v ok=%v", calls, ok)
	}
}
func TestParseModelToolDecisionNoCall(t *testing.T) {
	calls, ok := parseModelToolDecision(`{"calls":[]}`, testTools(), "auto")
	if !ok || len(calls) != 0 {
		t.Fatalf("calls=%v ok=%v", calls, ok)
	}
}
func TestModelToolRouterPromptMarksCompletedResults(t *testing.T) {
	p := modelToolRouterPrompt(`assistant tool_calls: [...]
tool[call_x]: 2026-07-18`, testTools(), "auto")
	if !strings.Contains(p, "Completed evidence must not be repeated") || !strings.Contains(p, "tool[call_x]: 2026-07-18") || !strings.Contains(p, "unfinished work remains") {
		t.Fatalf("missing multi-turn evidence constraint: %s", p)
	}
}

func TestModelToolRouterPromptAppendsExecutionAnchor(t *testing.T) {
	anchor := executionAnchorForPrompt("[system]\nPlatform: win32\nPrimary working directory: D:\\NetPeek")
	p := modelToolRouterPrompt("request", testTools(), "auto", anchor)
	if !strings.Contains(p, "EXECUTION ENVIRONMENT (caller-provided):") {
		t.Fatalf("router prompt missing execution anchor: %s", p)
	}
	if strings.Index(p, "EXECUTION ENVIRONMENT (caller-provided):") < strings.Index(p, "User request and evidence:") {
		t.Fatalf("anchor must follow user evidence: %s", p)
	}
}

func TestModelToolRouterPromptWithoutAnchor(t *testing.T) {
	p := modelToolRouterPrompt("request", testTools(), "auto")
	if strings.Contains(p, "EXECUTION ENVIRONMENT (caller-provided):") {
		t.Fatalf("plain router prompt must not contain anchor: %s", p)
	}
}

func TestParseModelToolDecisionRejectsBadSchema(t *testing.T) {
	calls, ok := parseModelToolDecision("```json\n{\"calls\":[{\"name\":\"get_weather\",\"arguments\":{\"city\":2}}]}\n```", testTools(), "auto")
	if !ok || len(calls) != 0 {
		t.Fatalf("calls=%v ok=%v", calls, ok)
	}
}

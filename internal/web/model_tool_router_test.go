package web

import (
	"encoding/json"
	"strings"
	"testing"

	"m365-copilot2api/internal/chathub"
)

func TestParseModelToolDecisionAutoAndParallel(t *testing.T) {
	calls, ok := parseModelToolDecision(`{"calls":[{"name":"get_weather","arguments":{"city":"Beijing"}},{"name":"get_time","arguments":{"city":"Beijing"}}]}`, testTools(), "auto")
	if !ok || len(calls) != 2 {
		t.Fatalf("calls=%v ok=%v", calls, ok)
	}
}

// Regression: models occasionally emit the same call twice in one envelope
// (historically encouraged by tools being exposed through both an API plugin
// and a self-referential MCP gateway). Each duplicate used to become a
// separate tool_call — two identical subtasks for the client.
func TestParseModelToolDecisionDeduplicatesIdenticalCalls(t *testing.T) {
	calls, ok := parseModelToolDecision(`{"calls":[
		{"name":"get_weather","arguments":{"city":"Beijing"}},
		{"name":"get_weather","arguments":{"city":"Beijing"}},
		{"name":"get_time","arguments":{"city":"Beijing"}},
		{"name":"get_weather","arguments":{"city":"Shanghai"}}
	]}`, testTools(), "auto")
	if !ok {
		t.Fatal("expected a parsed decision")
	}
	if len(calls) != 3 {
		t.Fatalf("expected duplicates to be dropped, got %d calls: %+v", len(calls), calls)
	}
	seen := map[string]bool{}
	for _, c := range calls {
		key := c.Name + string(c.Arguments)
		if seen[key] {
			t.Fatalf("duplicate call survived: %s %s", c.Name, c.Arguments)
		}
		seen[key] = true
	}
	if calls[0].ID == calls[1].ID {
		t.Fatalf("call IDs must remain unique after dedup: %s", calls[0].ID)
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

// The router decides whether a tool is called at all. Without the guard a
// model that believes it runs in a Linux container answers NO_TOOL_NEEDED for
// Windows paths, and the turn degrades into prose with no execution.
func TestModelToolRouterPromptPrependsExecutionGuard(t *testing.T) {
	p := modelToolRouterPrompt("request", testTools(), "auto")
	if !strings.Contains(p, "/mnt/data") || !strings.Contains(p, "not mounted") {
		t.Fatalf("router prompt missing Windows execution guard: %s", p)
	}
	// Primacy matters: the guard must precede both the role line and the
	// (potentially very large) evidence block.
	if strings.Index(p, "/mnt/data") > strings.Index(p, "You are a tool selection assistant.") {
		t.Fatalf("guard must precede the role line: %s", p)
	}
	if strings.Index(p, "/mnt/data") > strings.Index(p, "User request and evidence:") {
		t.Fatalf("guard must precede the evidence block: %s", p)
	}
}

// No declared tools means nothing can be executed on the caller's machine, so
// routing must not be primed with an execution contract.
func TestModelToolRouterPromptWithoutToolsOmitsGuard(t *testing.T) {
	p := modelToolRouterPrompt("request", nil, "auto")
	if strings.Contains(p, "/mnt/data") {
		t.Fatalf("tool-less router prompt must not carry the execution guard: %s", p)
	}
}

func TestParseModelToolDecisionRejectsBadSchema(t *testing.T) {
	calls, ok := parseModelToolDecision("```json\n{\"calls\":[{\"name\":\"get_weather\",\"arguments\":{\"city\":2}}]}\n```", testTools(), "auto")
	if !ok || len(calls) != 0 {
		t.Fatalf("calls=%v ok=%v", calls, ok)
	}
}

// The router picks one next step, so "the call that does the most work" is
// structurally always the subagent launcher — fresh conversations opened an
// Agent as their very first decision for tasks direct tools could do. The
// prompt must counter this bias and offer the parallel envelope instead.
func TestModelToolRouterPromptCountersDelegationBias(t *testing.T) {
	tools := append(testTools(), map[string]any{"type": "function", "function": map[string]any{"name": "Agent", "description": "launch a subagent", "parameters": map[string]any{"type": "object", "properties": map[string]any{"prompt": map[string]any{"type": "string"}}}}})
	p := modelToolRouterPrompt("request", tools, "auto")
	if !strings.Contains(p, "your own judgment call") || !strings.Contains(p, "tool's own description") {
		t.Fatalf("delegation-bias rule missing: %s", p)
	}
	if !strings.Contains(p, `{"calls":[{"name":"...","arguments":{...}}]}`) {
		t.Fatalf("parallel envelope hint missing: %s", p)
	}
	plain := modelToolRouterPrompt("request", testTools(), "auto")
	if strings.Contains(plain, "your own judgment call") {
		t.Fatal("bias rule must not appear without a delegation tool")
	}
}

// answerToolProtocol gives the ANSWER turn the same CALL_TOOL contract the
// router has, so a router NO_TOOL_NEEDED misjudgment can no longer structurally
// force a text-only "status report" answer (2026-09-10).
func TestAnswerToolProtocolContent(t *testing.T) {
	schema, _ := json.Marshal(map[string]any{
		"name":        "get_weather",
		"description": "weather lookup",
		"parameters":  map[string]any{"type": "object", "required": []any{"city"}, "properties": map[string]any{"city": map[string]any{"type": "string"}}},
	})
	tools := []chathub.Tool{{Type: "function", Function: schema}}
	ep := answerToolProtocol(tools)
	if ep == "" {
		t.Fatal("expected an epilogue for a tool-bearing answer turn")
	}
	for _, want := range []string{"TOOL EXECUTION PROTOCOL", "CALL_TOOL: ", `{"calls"`, "get_weather", "city"} {
		if !strings.Contains(ep, want) {
			t.Fatalf("epilogue missing %q", want)
		}
	}
	// The compact catalogue, not raw JSON schemas: schema keywords must not leak.
	for _, banned := range []string{`"required"`, `"properties"`} {
		if strings.Contains(ep, banned) {
			t.Fatalf("epilogue must carry the compact catalogue, found raw schema key %s", banned)
		}
	}
	if answerToolProtocol(nil) != "" {
		t.Fatal("no tools must produce no epilogue")
	}
	// The WindowsExecutionGuard must ride on the answer turn: without it a
	// switched upstream model denied having any tools (2026-09-10).
	for _, want := range []string{"executed by the caller on its own Windows machine", "NEVER claim that no callable tools exist"} {
		if !strings.Contains(ep, want) {
			t.Fatalf("epilogue missing guard clause %q", want)
		}
	}
}

// userMessageDemandsAction + the refusal phrase table: the 2026-09-10
// gpt-5.6-sol refusal ("当前会话没有可调用的 Windows 工作区文件编辑工具", 780
// chars) escaped both the legacy short-text isToolRefusal cap and its phrase
// list. The router rules must also forbid NO_TOOL_NEEDED on action requests.
func TestToolDenialDetection(t *testing.T) {
	long := strings.Repeat("需要落地。", 100) + "当前会话没有可调用的 Windows 工作区文件编辑工具，因此我无法安全地把改动写入。"
	if !containsToolDenial(long) {
		t.Fatal("containsToolDenial must match denial buried in a long reply")
	}
	if isToolRefusal(long) {
		t.Fatal("legacy isToolRefusal keeps its short-text cap for compat")
	}
	if !containsToolDenial("There are no callable tools in this session.") {
		t.Fatal("english denial not matched")
	}
	if containsToolDenial("All tools completed successfully.") {
		t.Fatal("false positive on a normal completion")
	}
	if !userMessageDemandsAction("实现以上未落地的内容") {
		t.Fatal("chinese execution request not detected")
	}
	if !userMessageDemandsAction("Please implement the missing pieces") {
		t.Fatal("english execution request not detected")
	}
	if userMessageDemandsAction("谢谢你，今天辛苦了") {
		t.Fatal("pure chat must not demand action")
	}
	rp := modelToolRouterPrompt("request", testTools(), "auto")
	if !strings.Contains(rp, "NO_TOOL_NEEDED is forbidden") {
		t.Fatal("router rules missing action-request ban")
	}
}

func TestClassifyAnswerOutputPrefix(t *testing.T) {
	cases := []struct {
		in                 string
		decided, isCall    bool
	}{
		{"", false, false},
		{"   \n", false, false},
		{"```", false, false},
		{"```json\n", false, false},
		{"{", false, false},
		{`{"name":"x"`, false, false},
		{`CALL_TOOL: get_weather({"city":"Paris"})`, true, true},
		{`call_tool: get_weather({"city":"Paris"})`, true, true},
		{"```json\n{\"calls\":[{\"name\":\"get_weather\",\"arguments\":{\"city\":\"Paris\"}}]}\n```", true, true},
		{`{"calls":[{"name":"get_weather","arguments":{"city":"Paris"}}]}`, true, true},
		{"好的，我来总结一下当前的进度……", true, false},
		{`The result is {"x":1} and more`, true, false},
	}
	for i, c := range cases {
		decided, isCall := classifyAnswerOutputPrefix(c.in)
		if decided != c.decided || (decided && isCall != c.isCall) {
			t.Fatalf("case %d %q: got decided=%v isCall=%v, want decided=%v isCall=%v", i, c.in, decided, isCall, c.decided, c.isCall)
		}
	}
}

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
		in              string
		decided, isCall bool
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

// toolRouterName extracts the names a router catalogue would show, so tests can
// assert on presence/absence without depending on catalogue ordering.
func toolRouterName(t map[string]any) string {
	f, _ := t["function"].(map[string]any)
	if f == nil {
		f = t
	}
	n, _ := f["name"].(string)
	return n
}

func toolRouterNames(tools []map[string]any) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, toolRouterName(t))
	}
	return out
}

func hasName(tools []map[string]any, want string) bool {
	for _, t := range tools {
		if toolRouterName(t) == want {
			return true
		}
	}
	return false
}

func planModeToolset() []map[string]any {
	return []map[string]any{
		{"type": "function", "function": map[string]any{"name": "Read", "description": "read a file"}},
		{"type": "function", "function": map[string]any{"name": "Edit", "description": "edit a file"}},
		{"type": "function", "function": map[string]any{"name": "EnterPlanMode", "description": "switch the caller into plan mode"}},
		{"type": "function", "function": map[string]any{"name": "ExitPlanMode", "description": "leave plan mode with a plan"}},
		{"name": "Agent", "description": "delegation as a bare tool object"},
	}
}

// Regression, 2026-09-11 00:56 live traffic: the user typed
// "实现以上未落地的内容", the router answered CALL_TOOL: EnterPlanMode({}), the
// caller switched into plan mode, and the turn ended as a plan with zero
// execution (the router then spent a further 28s turn on ExitPlanMode to
// escape). An execution request must never spend its single routing decision on
// the one tool that can only halt the work.
func TestRouterWithholdsPlanModeEntryOnExecutionRequest(t *testing.T) {
	filtered := planModeEligibleTools(planModeToolset(), "实现以上未落地的内容")
	if hasName(filtered, "EnterPlanMode") {
		t.Fatalf("EnterPlanMode survived an execution request: %v", toolRouterNames(filtered))
	}
	// ExitPlanMode is the escape hatch for an execution request that lands
	// while the caller is already in plan mode — dropping it would trap the
	// session in the very mode this change is trying to avoid.
	for _, keep := range []string{"ExitPlanMode", "Read", "Edit", "Agent"} {
		if !hasName(filtered, keep) {
			t.Fatalf("%s must stay routable, got %v", keep, toolRouterNames(filtered))
		}
	}
	// The catalogue the router actually reads must agree.
	p := modelToolRouterPrompt("实现以上未落地的内容", filtered, "auto")
	if strings.Contains(p, "EnterPlanMode") {
		t.Fatalf("router prompt still advertises EnterPlanMode: %s", p)
	}
	if !strings.Contains(p, "ExitPlanMode") {
		t.Fatalf("router prompt lost ExitPlanMode: %s", p)
	}
}

// The filter is scoped to the router's candidate list only: a request that is
// not an execution request keeps its full toolset, so a user who really did ask
// for a plan first can still get plan mode.
func TestPlanModeEntrySurvivesNonExecutionRequest(t *testing.T) {
	tools := planModeToolset()
	kept := planModeEligibleTools(tools, "这个项目的整体架构怎么样？")
	if !hasName(kept, "EnterPlanMode") {
		t.Fatalf("non-execution request must keep EnterPlanMode: %v", toolRouterNames(kept))
	}
	if len(kept) != len(tools) {
		t.Fatalf("toolset must be untouched, got %d want %d", len(kept), len(tools))
	}
}

func TestPlanModeEntryNameNormalization(t *testing.T) {
	for _, yes := range []string{"EnterPlanMode", "enter_plan_mode", "ENTER-PLAN-MODE", " planmode ", "StartPlanMode"} {
		if !isPlanModeEntryTool(yes) {
			t.Fatalf("%q must be recognised as a plan-mode entry tool", yes)
		}
	}
	for _, no := range []string{"ExitPlanMode", "Plan", "Agent", "Edit", "TodoWrite", "EnterPlanModeX", ""} {
		if isPlanModeEntryTool(no) {
			t.Fatalf("%q must not be treated as a plan-mode entry tool", no)
		}
	}
}

// The answer turn is the fallback when the router said NO_TOOL_NEEDED. If its
// textual CALL_TOOL catalogue still offers EnterPlanMode, a misjudged turn can
// end as a plan (the exact user-visible symptom) even after the router is fixed.
func TestAnswerTurnWithholdsPlanModeEntryOnExecutionRequest(t *testing.T) {
	mk := func(name string) chathub.Tool {
		raw, _ := json.Marshal(map[string]any{"name": name, "description": name + " tool", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}})
		return chathub.Tool{Type: "function", Function: raw}
	}
	defs := []chathub.Tool{mk("Read"), mk("EnterPlanMode"), mk("ExitPlanMode")}

	filtered := planModeEligibleToolDefs(defs, "实现以上未落地的内容")
	ep := answerToolProtocol(filtered)
	if ep == "" {
		t.Fatal("expected a tool protocol epilogue")
	}
	if strings.Contains(ep, "EnterPlanMode") {
		t.Fatalf("answer-turn protocol still advertises EnterPlanMode: %s", ep)
	}
	if !strings.Contains(ep, "ExitPlanMode") || !strings.Contains(ep, "Read") {
		t.Fatalf("answer-turn protocol dropped a legitimate tool: %s", ep)
	}
	// Non-execution requests keep the full protocol.
	full := answerToolProtocol(planModeEligibleToolDefs(defs, "这个项目的整体架构怎么样？"))
	if !strings.Contains(full, "EnterPlanMode") {
		t.Fatalf("non-execution request must keep the plan-mode tool: %s", full)
	}
}

func TestPlanModeFilterKillSwitch(t *testing.T) {
	t.Setenv("M365_ROUTER_ALLOW_PLAN_MODE", "true")
	kept := planModeEligibleTools(planModeToolset(), "实现以上未落地的内容")
	if !hasName(kept, "EnterPlanMode") {
		t.Fatalf("M365_ROUTER_ALLOW_PLAN_MODE=true must restore the previous behaviour: %v", toolRouterNames(kept))
	}
}

func TestRouterPromptWarnsPlanModeIsNotWork(t *testing.T) {
	p := modelToolRouterPrompt("request", planModeToolset(), "auto")
	if !strings.Contains(p, "EnterPlanMode is not work") {
		t.Fatalf("plan-mode counter-bias rule missing: %s", p)
	}
	// The rule names a tool, so it must not appear when that tool is absent.
	plain := modelToolRouterPrompt("request", testTools(), "auto")
	if strings.Contains(plain, "EnterPlanMode is not work") {
		t.Fatal("plan-mode rule must not appear without a plan-mode tool")
	}
}

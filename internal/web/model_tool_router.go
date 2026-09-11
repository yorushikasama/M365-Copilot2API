package web

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"m365-copilot2api/internal/chathub"
)

// maxRouterToolDescChars caps one tool's description in the router catalogue.
const maxRouterToolDescChars = 300

// routerToolCatalogue renders the tool list for the router turn.
//
// A coding-agent toolset ships 50+ complete JSON schemas; marshalled, that is
// 100KB+ uploaded on every single tool decision — on 2026-09-10 it was the
// larger half of a 213KB route prompt (the flattened history was 98KB), and it
// is re-uploaded for every router turn of every request. Routing only needs the
// tool's name, what it does, and which arguments it takes: the model still
// emits concrete arguments, and validateDetectedToolCalls checks them against
// the real schemas afterwards.
//
// Set M365_ROUTER_FULL_TOOL_SCHEMAS=true to restore the verbose catalogue.
func routerToolCatalogue(tools []map[string]any) []map[string]any {
	if len(tools) == 0 || os.Getenv("M365_ROUTER_FULL_TOOL_SCHEMAS") == "true" {
		return tools
	}
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		f, _ := t["function"].(map[string]any)
		if f == nil {
			f = t
		}
		name, _ := f["name"].(string)
		if name == "" {
			// Nothing to route without a name; keep the original entry.
			out = append(out, t)
			continue
		}
		entry := map[string]any{"name": name}
		if d, _ := f["description"].(string); d != "" {
			d = strings.TrimSpace(strings.Join(strings.Fields(d), " "))
			if len(d) > maxRouterToolDescChars {
				d = d[:maxRouterToolDescChars] + "…"
			}
			entry["desc"] = d
		}
		if args := routerToolArgs(f["parameters"]); len(args) > 0 {
			entry["args"] = args
		}
		out = append(out, entry)
	}
	return out
}

// routerToolArgs reduces a JSON schema to "argument name -> type", marking
// required arguments with a trailing "*" and listing the fields of object
// arguments so the model can still build them.
func routerToolArgs(params any) map[string]string {
	p, _ := params.(map[string]any)
	if p == nil {
		return nil
	}
	props, _ := p["properties"].(map[string]any)
	if len(props) == 0 {
		return nil
	}
	required := map[string]bool{}
	if list, ok := p["required"].([]any); ok {
		for _, v := range list {
			if s, ok := v.(string); ok {
				required[s] = true
			}
		}
	}
	args := make(map[string]string, len(props))
	for k, v := range props {
		typ := "any"
		if m, ok := v.(map[string]any); ok {
			if tv, ok := m["type"].(string); ok && tv != "" {
				typ = tv
			}
			if nested, ok := m["properties"].(map[string]any); ok && len(nested) > 0 {
				keys := make([]string, 0, len(nested))
				for k2 := range nested {
					keys = append(keys, k2)
				}
				sort.Strings(keys)
				if len(keys) > 8 {
					keys = keys[:8]
				}
				typ += "{" + strings.Join(keys, ",") + "}"
			}
		}
		if required[k] {
			typ += "*"
		}
		args[k] = typ
	}
	return args
}

// answerToolProtocol appends a compact tool-execution contract to the ANSWER
// turn prompt. new-api never needs an equivalent: its upstreams do native
// function calling, so the model that reads the history is also the model that
// decides tool calls. Our Copilot upstream has no tool channel, so the answer
// turn was text-only and the router alone decided whether work happened — a
// router misjudgment (2026-09-10: a compacted "继续完成未完成的内容" turn answered
// NO_TOOL_NEEDED) structurally produced a status report instead of execution.
// Giving the answering brain the same CALL_TOOL protocol restores the new-api
// convergence: it can execute the next step itself, and the router degrades
// to a latency optimization instead of a correctness gate.
func answerToolProtocol(tools []chathub.Tool) string {
	if len(tools) == 0 {
		return ""
	}
	maps := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		var f map[string]any
		_ = json.Unmarshal(tool.Function, &f)
		maps = append(maps, map[string]any{"type": tool.Type, "function": f})
	}
	defs, _ := json.Marshal(routerToolCatalogue(maps))
	// The WindowsExecutionGuard must ride on the ANSWER turn too, not only the
	// router turn: 2026-09-10 a switched upstream model (gpt-5.6-sol) answered
	// an execution request with "当前会话没有可调用的 Windows 工作区文件编辑工具" —
	// the classic sandbox hallucination the router guard already documents.
	// The answer prompt has no native tool schemas, so without the guard the
	// model trusts its own world view over the textual protocol and refuses.
	return fmt.Sprintf(`

%s

TOOL EXECUTION PROTOCOL: The tool list below IS this session's real, complete toolset — the caller (a coding agent on the user's Windows machine) executes whatever you emit here. The absence of native JSON tool schemas in this prompt is a transport detail of the gateway, NOT evidence that tools are missing. NEVER claim that no callable tools exist, that you cannot edit or write files, or that the workspace is unavailable — that statement would be factually wrong in this session.
Available tools: %s
- To execute, your ENTIRE reply must be exactly one call starting at the very first character: CALL_TOOL: tool_name({"arg":"value"}) — no prose before or after it
- Several independent steps: reply with only one JSON block {"calls":[{"name":"...","arguments":{...}}]}
- Use exactly the argument names listed (a trailing * marks a required argument); never invent tools that are not listed
- If the user asks to continue, finish, or complete work and a tool can advance it, emit the tool call — do not answer with a list of unverified items
- If you answer in prose instead, NEVER mention CALL_TOOL, {"calls" or any protocol marker in the text`, chathub.WindowsExecutionGuard, string(defs))
}

// actionDemandMarkers are substrings whose presence in the user's latest
// message marks it as an execution request rather than a question. Both the
// router and the answer turn use this to resist answering "I have no tools"
// or NO_TOOL_NEEDED when the user clearly asked for work to be done.
var actionDemandMarkers = []string{
	// English
	"implement", "fix ", "fix the", "fix it", "write ", "edit ", "create ", "delete ", "remove ", "run ", "execute", "apply ", "update ", "refactor", "modify", "install", "add ", "rename", "move ", "generate", "build ", "clean up", "complete the", "finish the", "continue",
	// Chinese
	"实现", "修改", "修复", "写入", "创建", "删除", "移除", "运行", "执行", "落地", "编辑", "更新", "重构", "构建", "安装", "重命名", "清理", "补齐", "完成未完成", "继续完成", "应用", "生成",
}

// userMessageDemandsAction reports whether the user's latest message is an
// execution request. It is a bias signal, not a gate: the answer turn may
// still legitimately answer in prose when no listed tool can advance the work.
func userMessageDemandsAction(text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	low := strings.ToLower(text)
	for _, m := range actionDemandMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// looksLikeToolRefusal is the streaming-path alias for containsToolDenial
// (toolloop.go owns the phrase table so both paths cannot drift).
func looksLikeToolRefusal(text string) bool { return containsToolDenial(text) }

// planModeEntryNames are the separator-stripped, lowercased names of "enter
// plan mode" tools — meta tools that HALT execution and hand the turn back to
// the caller's planning UI. They are the worst possible answer to an execution
// request while looking like the most decisive call available.
var planModeEntryNames = map[string]bool{
	"enterplanmode": true,
	"startplanmode": true,
	"planmode":      true,
}

// normalizeToolName lowercases a tool name and strips separators so one tool
// spelled EnterPlanMode / enter_plan_mode / enter-plan-mode matches once.
func normalizeToolName(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return ""
	}
	return strings.NewReplacer("_", "", "-", "", " ", "", ".", "").Replace(n)
}

// isPlanModeEntryTool reports whether name is an "enter plan mode" meta tool.
func isPlanModeEntryTool(name string) bool {
	return planModeEntryNames[normalizeToolName(name)]
}

// toolFunctionName reads a tool's name from either shape the gateway carries:
// the OpenAI envelope {"type":"function","function":{...,"name":...}} or a bare
// {"name":...} object.
func toolFunctionName(t map[string]any) string {
	f, _ := t["function"].(map[string]any)
	if f == nil {
		f = t
	}
	name, _ := f["name"].(string)
	return name
}

func hasPlanModeEntryTool(tools []map[string]any) bool {
	for _, t := range tools {
		if isPlanModeEntryTool(toolFunctionName(t)) {
			return true
		}
	}
	return false
}

// withoutPlanModeEntry drops "enter plan mode" tools from a tool list. Entering
// plan mode is a halt, never a step of the work: the router is asked for the ONE
// next call that advances the request, and this tool advances nothing.
//
// The regression this closes (2026-09-11 00:56 live): the user's message was
// "实现以上未落地的内容", the router answered CALL_TOOL: EnterPlanMode({}), the
// caller entered plan mode, and the turn ended as a plan with zero execution —
// then the router had to spend another 28s turn on ExitPlanMode to escape.
// ExitPlanMode is deliberately NOT dropped: it is the legitimate escape hatch
// for an execution request that arrives while the caller is already in plan
// mode.
//
// Set M365_ROUTER_ALLOW_PLAN_MODE=true to disable the filter.
func withoutPlanModeEntry(tools []map[string]any) []map[string]any {
	if len(tools) == 0 || os.Getenv("M365_ROUTER_ALLOW_PLAN_MODE") == "true" {
		return tools
	}
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		if isPlanModeEntryTool(toolFunctionName(t)) {
			continue
		}
		out = append(out, t)
	}
	if len(out) == len(tools) {
		return tools
	}
	return out
}

// withoutPlanModeEntryTools is the []chathub.Tool twin of withoutPlanModeEntry,
// used for the answer turn's textual tool protocol (answerToolProtocol).
func withoutPlanModeEntryTools(tools []chathub.Tool) []chathub.Tool {
	if len(tools) == 0 || os.Getenv("M365_ROUTER_ALLOW_PLAN_MODE") == "true" {
		return tools
	}
	out := make([]chathub.Tool, 0, len(tools))
	dropped := false
	for _, t := range tools {
		var f struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(t.Function, &f)
		if isPlanModeEntryTool(f.Name) {
			dropped = true
			continue
		}
		out = append(out, t)
	}
	if !dropped {
		return tools
	}
	return out
}

// planModeEligibleTools applies the plan-mode-entry policy for one request: when
// the user's latest message is an execution request, the tool that can only halt
// the work is withheld from both the router catalogue and the answer turn's
// CALL_TOOL protocol. When the message is not an execution request (the user
// really did ask to plan), the toolset is untouched.
func planModeEligibleTools(tools []map[string]any, lastUserText string) []map[string]any {
	if !userMessageDemandsAction(lastUserText) {
		return tools
	}
	return withoutPlanModeEntry(tools)
}

// planModeEligibleToolDefs is the []chathub.Tool twin of planModeEligibleTools,
// for the answer turn's textual tool protocol.
func planModeEligibleToolDefs(tools []chathub.Tool, lastUserText string) []chathub.Tool {
	if !userMessageDemandsAction(lastUserText) {
		return tools
	}
	return withoutPlanModeEntryTools(tools)
}

func modelToolRouterPrompt(prompt string, tools []map[string]any, choice any, anchors ...string) string {
	defs, _ := json.Marshal(routerToolCatalogue(tools))
	mode := normalizedToolChoiceMode(choice)
	rules := `- If a tool is needed, respond with: CALL_TOOL: tool_name({"arg1":"value1"})
- If no tool is needed, respond with: NO_TOOL_NEEDED
- Only use tools from the available list above
- Use exactly the argument names listed for the tool (a trailing * marks a required argument)
- Do not invent tools that are not in the list
- If the user's latest message is an execution request (implement, fix, write, edit, create, run, continue, 实现, 修改, 修复, 写入, 创建, 运行, 继续完成...), NO_TOOL_NEEDED is forbidden unless the whole task is purely conversational: pick the listed tool that advances the work. Denying that tools exist is always wrong — the caller executes your emitted calls on its own machine`
	// The router decides ONE next step, which structurally biases the model
	// toward the delegation tool: "the single call that does the most work"
	// is always the subagent launcher, so fresh conversations routinely opened
	// an Agent as their very first decision for tasks direct tools could do.
	// Counter the bias explicitly and teach the parallel envelope so several
	// direct steps stay direct calls instead of being bundled into a subagent.
	if hasDelegationTool(tools) {
		rules += `
- A subagent/orchestrator tool is your own judgment call, made by comparing the work against the tool's own description: delegate only when the task genuinely matches (a large, self-contained subtask); users rarely ask for delegation explicitly, and routine step-by-step work must stay on direct tools
- Several independent direct steps must be parallel direct calls, never bundled into a subagent: respond with one JSON code block {"calls":[{"name":"...","arguments":{...}}]}`
	}
	// Plan-mode entry is a halt, not a step. It reads as "the most decisive
	// call" while executing nothing, so a request to do work must not spend
	// its one decision on it.
	if hasPlanModeEntryTool(tools) {
		rules += `
- EnterPlanMode is not work: it halts execution and returns a plan instead. Never select it for an execution request — select the tool that actually reads, edits, writes or runs something. Select it only when the user explicitly asked for a plan before any change`
	}
	// Multi-turn: completed tool evidence (tool[...], tool_calls:) was already
	// acted upon, so re-invoking those tools would duplicate work.
	if strings.Contains(prompt, "tool_calls:") || strings.Contains(prompt, "tool[call_") {
		rules += `
- Completed evidence must not be repeated: tool_calls/tool[call_x] rows are prior results already delivered to the user, never re-invoke them
- Only start a new tool call when fresh unfinished work remains on the current request`
	}
	// The router decides whether to call a tool *at all*, which makes a sandbox
	// hallucination fatal here: a model that believes it sits in a Linux
	// container with no access to D:\ concludes no tool can help and answers
	// NO_TOOL_NEEDED, so the turn degrades into prose with no execution.
	// Prepend the same caller-side contract the answer turn uses; it must come
	// first because the evidence block below can be tens of KB.
	guard := ""
	if len(tools) > 0 {
		guard = chathub.WindowsExecutionGuard + "\n\n"
	}
	result := fmt.Sprintf(`%sYou are a tool selection assistant. Based on the user request, decide which tool to call next.

Available tools: %s

MODE: %s

Rules:
%s

User request and evidence:
%s`, guard, defs, mode, rules, prompt)
	if len(anchors) > 0 {
		result = appendExecutionAnchor(result, anchors[0])
	}
	return result
}

// hasDelegationTool reports whether the tool list contains an orchestrator /
// subagent launcher (Agent, Task, *subagent*, *delegate*, *spawn*). These
// names follow the OpenAI/Codex conventions; anything else is treated as a
// direct tool.
func hasDelegationTool(tools []map[string]any) bool {
	for _, t := range tools {
		f, _ := t["function"].(map[string]any)
		n, _ := f["name"].(string)
		low := strings.ToLower(n)
		if low == "" {
			continue
		}
		if low == "agent" || low == "task" || strings.Contains(low, "subagent") || strings.Contains(low, "delegate") || strings.Contains(low, "spawn") {
			return true
		}
	}
	return false
}

// classifyAnswerOutputPrefix inspects the opening bytes of an answer-turn
// stream to decide whether the model is emitting the tool protocol (a
// CALL_TOOL line or the {"calls":[...]} envelope) or prose. It returns
// decided=false while the prefix is still ambiguous (blank, a fence opener, a
// bare "{"), so the caller keeps buffering, and decided=true with isCall when
// the disposition is known. Leading whitespace, a ``` fence and an optional
// "json" tag are skipped before matching.
func classifyAnswerOutputPrefix(s string) (decided, isCall bool) {
	t := strings.TrimSpace(s)
	t = strings.TrimPrefix(t, "```")
	t = strings.TrimSpace(t)
	t = strings.TrimPrefix(t, "json")
	t = strings.TrimSpace(t)
	if t == "" {
		return false, false
	}
	low := strings.ToLower(t)
	if strings.HasPrefix(low, "call_tool:") || strings.HasPrefix(low, "call_tool：") {
		return true, true
	}
	// Still a viable prefix of the marker: keep buffering. Upstream deltas are
	// tiny (the measured first delta is 4-5 bytes), so a fragment such as "CALL"
	// must NOT decide "definitely not a call" — that decision released the
	// holdback and the caller saw the literal protocol line as content while the
	// gateway also emitted the parsed tool call (2026-09-11).
	if isProtocolMarkerPrefix(low) {
		return false, false
	}
	if strings.HasPrefix(t, "{") {
		comp := strings.NewReplacer(" ", "", "\t", "", "\n", "", "\r", "").Replace(t)
		if strings.HasPrefix(comp, `{"calls"`) {
			return true, true
		}
		return false, false // could still become the envelope; keep buffering
	}
	return true, false
}

func parseModelToolDecision(text string, tools []map[string]any, choice any) ([]detectedToolCall, bool) {
	text = strings.TrimSpace(text)
	// Try the new natural language format first: CALL_TOOL: name({...})
	if strings.HasPrefix(text, "CALL_TOOL:") || strings.HasPrefix(text, "call_tool:") {
		parts := strings.SplitN(text, ":", 2)
		if len(parts) == 2 {
			rest := strings.TrimSpace(parts[1])
			start := strings.Index(rest, "(")
			end := strings.LastIndex(rest, ")")
			if start > 0 && end > start {
				name := strings.TrimSpace(rest[:start])
				argsStr := rest[start+1 : end]
				var args map[string]any
				if json.Unmarshal([]byte(argsStr), &args) == nil && toolChoiceAllows(choice, name) {
					fn := toolFunction(name, tools)
					if fn != nil && schemaValid(args, fn) == nil {
						b, _ := json.Marshal(args)
						return []detectedToolCall{{ID: callID(name, string(b), 0), Type: toolType(name, tools), Name: name, Arguments: b}}, true
					}
				}
			}
		}
	}
	if strings.Contains(text, "NO_TOOL_NEEDED") || strings.Contains(text, "no_tool_needed") {
		return nil, true
	}
	// Fallback: try the old JSON format
	if i := strings.Index(text, "```"); i >= 0 {
		text = strings.TrimSpace(strings.TrimPrefix(strings.TrimSuffix(text[i+3:], "```"), "json"))
	}
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return nil, false
	}
	var probe map[string]json.RawMessage
	if json.Unmarshal([]byte(text[start:end+1]), &probe) != nil {
		return nil, false
	}
	if _, ok := probe["calls"]; !ok {
		return nil, false
	}
	var envelope struct {
		Calls []struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"calls"`
	}
	if json.Unmarshal([]byte(text[start:end+1]), &envelope) != nil {
		return nil, false
	}
	out := make([]detectedToolCall, 0, len(envelope.Calls))
	// Deduplicate identical (name, arguments) pairs. Models occasionally emit
	// the same call twice in one envelope — especially when tools were exposed
	// through more than one channel historically — and each duplicate became a
	// separate tool_call, i.e. two identical subtasks for the client.
	seen := make(map[string]bool, len(envelope.Calls))
	for _, c := range envelope.Calls {
		fn := toolFunction(c.Name, tools)
		if fn == nil || c.Arguments == nil || !toolChoiceAllows(choice, c.Name) || schemaValid(c.Arguments, fn) != nil {
			continue
		}
		b, _ := json.Marshal(c.Arguments)
		key := c.Name + "\x00" + string(b)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, detectedToolCall{ID: callID(c.Name, string(b), len(out)), Type: toolType(c.Name, tools), Name: c.Name, Arguments: b})
	}
	return out, true
}

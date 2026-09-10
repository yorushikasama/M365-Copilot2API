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

func modelToolRouterPrompt(prompt string, tools []map[string]any, choice any, anchors ...string) string {
	defs, _ := json.Marshal(routerToolCatalogue(tools))
	mode := normalizedToolChoiceMode(choice)
	rules := `- If a tool is needed, respond with: CALL_TOOL: tool_name({"arg1":"value1"})
- If no tool is needed, respond with: NO_TOOL_NEEDED
- Only use tools from the available list above
- Use exactly the argument names listed for the tool (a trailing * marks a required argument)
- Do not invent tools that are not in the list`
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

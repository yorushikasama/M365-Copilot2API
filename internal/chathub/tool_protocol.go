package chathub

import (
	"encoding/json"
	"fmt"
	"strings"
)

// windowsExecutionGuard is the caller-side execution contract: tools run on
// the caller's Windows machine, never in an upstream sandbox. Without it,
// upstream models drift into describing their own environment ("/mnt/data",
// "Linux container") and refuse to act on Windows paths.
//
// This guard used to be unreachable dead code, not merely rarely hit. It sat
// only in the branch that inlines tool schemas, which is guarded by
// hasPlugins == false. But clientPlugins() registers a plugin for every
// *valid* tool, and building a non-empty defs list requires at least one
// valid tool — so hasPlugins is true whenever defs is non-empty. The guard
// therefore required hasPlugins == false and len(defs) > 0 simultaneously,
// which is impossible. Both `if hasPlugins` early-returns skipped it, leaving
// only the weaker executionAnchor as live mitigation.
//
// Note the guard now applies to essentially every request: clientPlugins()
// falls back to a built-in plugin whenever tools are empty, and registers an
// mcp-gateway plugin whenever a gateway URL is configured, so hasPlugins is
// almost always true in practice.
const windowsExecutionGuard = "You are an execution agent on the caller's Windows machine. The tools available to you are real, active, and callable right now, and they are executed by the caller on its own machine, not by you in any remote environment. The bash tool runs Windows PowerShell 5.1; Windows paths like D:\\ are directly accessible. Do NOT use any built-in code interpreter, Python sandbox, or cloud execution environment. Do NOT emit backtick-backtick-backtick-python or backtick-backtick-backtick-code blocks for execution — if you need to run code, use the bash tool. Do NOT mention Linux containers, /mnt/data, cloud sandboxes, or claim the execution environment has changed. Do NOT claim the caller workspace is not mounted, is unmounted, or is otherwise unavailable or inaccessible."

// toolCallConvention explains how to emit a call when definitions are inlined
// into the prompt as a fenced schema.
const toolCallConvention = "When the user's request requires a tool, call it by emitting one or more fenced blocks. Each block's info string is the exact tool name and its body is a JSON object of arguments. For independent operations, emit multiple blocks in one response. Do not analyze whether tools are registered or available — they are. Do not say a tool is unavailable. Do not wrap the call in XML or Markdown prose. Wait for the tool result before claiming completion."

// toolProtocolPrompt follows the community-compatible M365 convention:
// definitions are wrapped in <tools>, and calls are emitted as a fenced block
// whose info string is the exact tool name.
func toolProtocolPrompt(text string, tools []Tool, choice any, hasPlugins bool, anchors ...string) string {
	anchor := ""
	if len(anchors) > 0 {
		anchor = anchors[0]
	}
	withAnchor := func(s string) string { return appendToolExecutionAnchor(s, anchor) }
	withGuard := func(s string) string { return windowsExecutionGuard + "\n\n" + s }
	if len(tools) == 0 || strings.EqualFold(fmt.Sprint(choice), "none") {
		if hasPlugins {
			return withAnchor(withGuard(text))
		}
		return withAnchor(fmt.Sprintf("Please answer the following request in full. Do not truncate or abbreviate your response.\n\n%s", text))
	}
	if hasPlugins {
		return withAnchor(withGuard(text))
	}
	var defs []string
	for _, t := range tools {
		var f struct {
			Name, Description string
			Parameters        json.RawMessage `json:"parameters"`
		}
		if json.Unmarshal(t.Function, &f) != nil || f.Name == "" {
			continue
		}
		params := strings.TrimSpace(string(f.Parameters))
		if params == "" || params == "null" {
			params = "{}"
		}
		defs = append(defs, fmt.Sprintf("%s — %s\n```%s\n%s\n```", f.Name, f.Description, f.Name, params))
	}
	if len(defs) == 0 {
		return withAnchor(withGuard(text))
	}
	return withAnchor(fmt.Sprintf("%s\n%s\n\n<tools>\n%s\n</tools>\n\nUser request:\n%s", windowsExecutionGuard, toolCallConvention, strings.Join(defs, "\n\n"), text))
}

func appendToolExecutionAnchor(text, anchor string) string {
	if strings.TrimSpace(anchor) == "" || strings.Contains(text, "EXECUTION ENVIRONMENT (caller-provided):") {
		return text
	}
	return strings.TrimSpace(text) + "\n\n" + anchor
}

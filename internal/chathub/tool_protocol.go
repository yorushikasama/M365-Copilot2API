package chathub

import (
	"encoding/json"
	"fmt"
	"strings"
)

// WindowsExecutionGuard is the caller-side execution contract: the declared
// tools run on the caller's Windows machine, never in an upstream sandbox.
// Without it, upstream models drift into describing their own environment
// ("/mnt/data", "Linux container", "workspace not mounted") and refuse to act
// on Windows paths. It is exported because the web layer needs the same
// contract in the tool-router prompt; keeping one copy prevents the two
// prompts from drifting apart.
//
// This guard used to be unreachable dead code, not merely rarely hit. It sat
// only in the branch that inlines tool schemas, which is guarded by
// HasPlugins == false. But clientPlugins() registers a plugin for every
// *valid* tool, and building a non-empty defs list requires at least one
// valid tool — so HasPlugins is true whenever defs is non-empty. The guard
// therefore required HasPlugins == false and len(defs) > 0 simultaneously,
// which is impossible. Both `if HasPlugins` early-returns skipped it, leaving
// only the weaker execution anchor as live mitigation.
const WindowsExecutionGuard = "The tools available to you are real, active, and callable right now, and they are executed by the caller on its own Windows machine, not by you in any remote environment. The bash tool runs Windows PowerShell 5.1; Windows paths like D:\\ are directly accessible. Do NOT use any built-in code interpreter, Python sandbox, or cloud execution environment. Do NOT emit backtick-backtick-backtick-python or backtick-backtick-backtick-code blocks for execution — if you need to run code, use the bash tool. Do NOT mention Linux containers, /mnt/data, cloud sandboxes, or claim the execution environment has changed. Do NOT claim the caller workspace is not mounted, is unmounted, or is otherwise unavailable or inaccessible."

// protocolCapabilities separates the two independent questions
// toolProtocolPrompt has to answer. Conflating them was the original defect:
// "plugins exist" is true even for plain chat, because clientPlugins() falls
// back to a built-in search plugin when no tools are declared, so it could
// never be used to decide whether the caller can execute anything.
type protocolCapabilities struct {
	// HasPlugins reports whether upstream will schedule tool calls through
	// registered plugins. When true the schemas are not inlined into the
	// prompt, because upstream already has them.
	HasPlugins bool
	// CanExecute reports whether the caller can actually execute something on
	// its own machine: caller-declared API tools or an MCP gateway. The
	// built-in search fallback is a plugin but not execution capability, so
	// an ordinary chat must not be primed as an agent turn.
	CanExecute bool
}

// toolCallConvention explains how to emit a call when definitions are inlined
// into the prompt as a fenced schema.
const toolCallConvention = "When the user's request requires a tool, call it by emitting one or more fenced blocks. Each block's info string is the exact tool name and its body is a JSON object of arguments. For independent operations, emit multiple blocks in one response. Do not analyze whether tools are registered or available — they are. Do not say a tool is unavailable. Do not wrap the call in XML or Markdown prose. Wait for the tool result before claiming completion."

// toolProtocolPrompt follows the community-compatible M365 convention:
// definitions are wrapped in <tools>, and calls are emitted as a fenced block
// whose info string is the exact tool name.
func toolProtocolPrompt(text string, tools []Tool, choice any, caps protocolCapabilities, anchors ...string) string {
	anchor := ""
	if len(anchors) > 0 {
		anchor = anchors[0]
	}
	withAnchor := func(s string) string { return appendToolExecutionAnchor(s, anchor) }
	// The guard is prepended, not appended: it sits before an 80KB+ prompt in
	// agent turns, and primacy matters more than wording here.
	guard := func(s string) string {
		if !caps.CanExecute {
			return s
		}
		return WindowsExecutionGuard + "\n\n" + s
	}
	if len(tools) == 0 || strings.EqualFold(fmt.Sprint(choice), "none") {
		if caps.HasPlugins {
			return withAnchor(guard(text))
		}
		return withAnchor(fmt.Sprintf("Please answer the following request in full. Do not truncate or abbreviate your response.\n\n%s", text))
	}
	if caps.HasPlugins {
		return withAnchor(guard(text))
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
	// A non-empty defs list means at least one valid tool was declared, which
	// is exactly the condition that makes CanExecute true, so the guard always
	// applies on the inlining path.
	prefix := ""
	if caps.CanExecute {
		prefix = WindowsExecutionGuard + "\n"
	}
	if len(defs) == 0 {
		// Every declared tool failed to parse, so there is no caller
		// capability either; guard()/prefix degrade to a no-op.
		return withAnchor(guard(text))
	}
	return withAnchor(fmt.Sprintf("%s%s\n\n<tools>\n%s\n</tools>\n\nUser request:\n%s", prefix, toolCallConvention, strings.Join(defs, "\n\n"), text))
}

func appendToolExecutionAnchor(text, anchor string) string {
	if strings.TrimSpace(anchor) == "" || strings.Contains(text, "EXECUTION ENVIRONMENT (caller-provided):") {
		return text
	}
	return strings.TrimSpace(text) + "\n\n" + anchor
}

package chathub

import (
	"encoding/json"
	"strings"
	"testing"
)

func toolProtocolTestTool() Tool {
	return Tool{Type: "function", Function: json.RawMessage(`{"name":"bash","description":"run a command","parameters":{"type":"object"}}`)}
}

func TestCallerCanExecute(t *testing.T) {
	valid := toolProtocolTestTool()
	broken := Tool{Type: "function", Function: json.RawMessage(`{not json`)}
	nameless := Tool{Type: "function", Function: json.RawMessage(`{"description":"no name"}`)}
	cases := []struct {
		name  string
		tools []Tool
		want  bool
	}{
		{"plain chat", nil, false},
		{"declared tool", []Tool{valid}, true},
		// A tool that cannot be parsed grants no capability: upstream gets no
		// usable schema for it either.
		{"unparseable tool only", []Tool{broken}, false},
		{"nameless tool only", []Tool{nameless}, false},
		{"one valid among broken", []Tool{broken, valid}, true},
	}
	for _, tc := range cases {
		if got := callerCanExecute(tc.tools); got != tc.want {
			t.Errorf("%s: callerCanExecute=%v want %v", tc.name, got, tc.want)
		}
	}
}

// The built-in search fallback makes "has plugins" true for an ordinary chat,
// so plugin presence alone must never be treated as execution capability.
func TestClientPluginsFallbackIsNotExecutionCapability(t *testing.T) {
	if got := len(clientPlugins(nil)); got == 0 {
		t.Fatal("clientPlugins must fall back to a built-in plugin for a tool-less request")
	}
	if callerCanExecute(nil) {
		t.Fatal("built-in search fallback must not count as caller execution capability")
	}
}

func TestToolProtocolPromptNoToolsKeepsAnchor(t *testing.T) {
	text := toolProtocolPrompt("hello", nil, nil, protocolCapabilities{}, "EXECUTION ENVIRONMENT (caller-provided): anchor")
	if !strings.Contains(text, "Please answer the following request in full") {
		t.Fatalf("expected full-answer prefix: %s", text)
	}
	if !strings.Contains(text, "EXECUTION ENVIRONMENT (caller-provided): anchor") {
		t.Fatalf("no-tools branch lost anchor: %s", text)
	}
}

func TestToolProtocolPromptChoiceNoneKeepsAnchor(t *testing.T) {
	text := toolProtocolPrompt("hello", []Tool{toolProtocolTestTool()}, "none", protocolCapabilities{}, "EXECUTION ENVIRONMENT (caller-provided): anchor")
	if strings.Contains(text, "<tools>") {
		t.Fatalf("choice=none must not emit tool schema: %s", text)
	}
	if !strings.Contains(text, "EXECUTION ENVIRONMENT (caller-provided): anchor") {
		t.Fatalf("choice=none branch lost anchor: %s", text)
	}
}

func TestToolProtocolPromptPluginsKeepsAnchorWithoutSchema(t *testing.T) {
	text := toolProtocolPrompt("hello", []Tool{toolProtocolTestTool()}, "auto", protocolCapabilities{HasPlugins: true, CanExecute: true}, "EXECUTION ENVIRONMENT (caller-provided): anchor")
	if strings.Contains(text, "<tools>") {
		t.Fatalf("plugin branch must not emit fenced schema: %s", text)
	}
	if !strings.Contains(text, "EXECUTION ENVIRONMENT (caller-provided): anchor") {
		t.Fatalf("plugin branch lost anchor: %s", text)
	}
	if !strings.Contains(text, "hello") {
		t.Fatalf("plugin branch dropped user text: %s", text)
	}
}

func TestToolProtocolPromptNoPluginsKeepsProtocolAndAnchor(t *testing.T) {
	text := toolProtocolPrompt("hello", []Tool{toolProtocolTestTool()}, "auto", protocolCapabilities{CanExecute: true}, "EXECUTION ENVIRONMENT (caller-provided): anchor")
	if !strings.Contains(text, "<tools>") || !strings.Contains(text, "```bash") {
		t.Fatalf("expected fenced tool protocol: %s", text)
	}
	if !strings.Contains(text, "EXECUTION ENVIRONMENT (caller-provided): anchor") {
		t.Fatalf("no-plugin branch lost anchor: %s", text)
	}
	if strings.Index(text, "EXECUTION ENVIRONMENT (caller-provided): anchor") < strings.Index(text, "User request:") {
		t.Fatalf("anchor must follow user request: %s", text)
	}
}

func TestToolProtocolPromptNoAnchorUnchanged(t *testing.T) {
	text := toolProtocolPrompt("hello", []Tool{toolProtocolTestTool()}, "auto", protocolCapabilities{CanExecute: true})
	if strings.Contains(text, "EXECUTION ENVIRONMENT (caller-provided):") {
		t.Fatalf("no anchor should not be injected: %s", text)
	}
	if !strings.Contains(text, "hello") {
		t.Fatalf("user text missing: %s", text)
	}
}

// Regression: with an MCP gateway configured, clientPlugins() always yields a
// plugin, so the HasPlugins branch is the production path. It used to skip the
// Windows execution guard entirely, which let upstream models claim the caller
// workspace was unmounted and that /mnt/data was empty.
func TestToolProtocolPromptPluginsEmitsExecutionGuard(t *testing.T) {
	text := toolProtocolPrompt("hello", []Tool{toolProtocolTestTool()}, "auto", protocolCapabilities{HasPlugins: true, CanExecute: true}, "EXECUTION ENVIRONMENT (caller-provided): anchor")
	if !strings.Contains(text, "/mnt/data") {
		t.Fatalf("plugin branch must still emit the Windows execution guard: %s", text)
	}
	if !strings.Contains(text, "not mounted") {
		t.Fatalf("plugin branch guard must forbid unmounted-workspace claims: %s", text)
	}
	if strings.Contains(text, "<tools>") {
		t.Fatalf("plugin branch must not emit fenced schema: %s", text)
	}
	if !strings.Contains(text, "EXECUTION ENVIRONMENT (caller-provided): anchor") {
		t.Fatalf("plugin branch lost anchor: %s", text)
	}
}

func TestToolProtocolPromptNoToolsWithPluginsEmitsExecutionGuard(t *testing.T) {
	// Degenerate-but-defensive shape: plugin presence with caller execution
	// capability even though no tools are inlined. The guard must still fire.
	text := toolProtocolPrompt("hello", nil, nil, protocolCapabilities{HasPlugins: true, CanExecute: true})
	if !strings.Contains(text, "/mnt/data") {
		t.Fatalf("mcp-gateway-only branch must emit the Windows execution guard: %s", text)
	}
}

func TestToolProtocolPromptNoToolsNoPluginsOmitsGuard(t *testing.T) {
	// Degenerate branch: no tools at all and no plugin fallback registered.
	text := toolProtocolPrompt("hello", nil, nil, protocolCapabilities{})
	if strings.Contains(text, "/mnt/data") {
		t.Fatalf("no-capability branch must not carry the execution guard: %s", text)
	}
}

// The defect this refactor fixes: a plain chat arrives with HasPlugins == true
// because clientPlugins() falls back to a built-in search plugin. Using that
// flag for the guard primed every ordinary conversation as an agent turn.
func TestToolProtocolPromptPlainChatWithPluginFallbackOmitsGuard(t *testing.T) {
	caps := protocolCapabilities{
		HasPlugins: len(clientPlugins(nil)) > 0, // true: built-in fallback
		CanExecute: callerCanExecute(nil),       // false: nothing to execute
	}
	if !caps.HasPlugins {
		t.Fatal("precondition: tool-less request must still report plugins")
	}
	text := toolProtocolPrompt("what is the weather today", nil, nil, caps)
	if strings.Contains(text, "/mnt/data") {
		t.Fatalf("plain chat must not be primed with the execution guard: %s", text)
	}
	if !strings.Contains(text, "what is the weather today") {
		t.Fatalf("user text missing: %s", text)
	}
}

// An unparseable tool list grants no capability, so no guard — matching
// upstream, which also receives no usable schema.
func TestToolProtocolPromptUnparseableToolsOmitsGuard(t *testing.T) {
	broken := Tool{Type: "function", Function: json.RawMessage(`{not json`)}
	caps := protocolCapabilities{
		HasPlugins: len(clientPlugins([]Tool{broken})) > 0,
		CanExecute: callerCanExecute([]Tool{broken}),
	}
	if caps.CanExecute {
		t.Fatal("precondition: broken tool must not grant capability")
	}
	text := toolProtocolPrompt("hello", []Tool{broken}, "auto", caps)
	if strings.Contains(text, "/mnt/data") {
		t.Fatalf("guard emitted without execution capability: %s", text)
	}
}

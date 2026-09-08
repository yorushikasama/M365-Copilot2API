package chathub

import (
	"encoding/json"
	"strings"
	"testing"
)

func toolProtocolTestTool() Tool {
	return Tool{Type: "function", Function: json.RawMessage(`{"name":"bash","description":"run a command","parameters":{"type":"object"}}`)}
}

func TestToolProtocolPromptNoToolsKeepsAnchor(t *testing.T) {
	text := toolProtocolPrompt("hello", nil, nil, false, "EXECUTION ENVIRONMENT (caller-provided): anchor")
	if !strings.Contains(text, "Please answer the following request in full") {
		t.Fatalf("expected full-answer prefix: %s", text)
	}
	if !strings.Contains(text, "EXECUTION ENVIRONMENT (caller-provided): anchor") {
		t.Fatalf("no-tools branch lost anchor: %s", text)
	}
}

func TestToolProtocolPromptChoiceNoneKeepsAnchor(t *testing.T) {
	text := toolProtocolPrompt("hello", []Tool{toolProtocolTestTool()}, "none", false, "EXECUTION ENVIRONMENT (caller-provided): anchor")
	if strings.Contains(text, "<tools>") {
		t.Fatalf("choice=none must not emit tool schema: %s", text)
	}
	if !strings.Contains(text, "EXECUTION ENVIRONMENT (caller-provided): anchor") {
		t.Fatalf("choice=none branch lost anchor: %s", text)
	}
}

func TestToolProtocolPromptPluginsKeepsAnchorWithoutSchema(t *testing.T) {
	text := toolProtocolPrompt("hello", []Tool{toolProtocolTestTool()}, "auto", true, "EXECUTION ENVIRONMENT (caller-provided): anchor")
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
	text := toolProtocolPrompt("hello", []Tool{toolProtocolTestTool()}, "auto", false, "EXECUTION ENVIRONMENT (caller-provided): anchor")
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
	text := toolProtocolPrompt("hello", []Tool{toolProtocolTestTool()}, "auto", false)
	if strings.Contains(text, "EXECUTION ENVIRONMENT (caller-provided):") {
		t.Fatalf("no anchor should not be injected: %s", text)
	}
	if !strings.Contains(text, "hello") {
		t.Fatalf("user text missing: %s", text)
	}
}

// Regression: with an MCP gateway configured, clientPlugins() always yields a
// plugin, so the hasPlugins branch is the production path. It used to skip the
// Windows execution guard entirely, which let upstream models claim the caller
// workspace was unmounted and that /mnt/data was empty.
func TestToolProtocolPromptPluginsEmitsExecutionGuard(t *testing.T) {
	text := toolProtocolPrompt("hello", []Tool{toolProtocolTestTool()}, "auto", true, "EXECUTION ENVIRONMENT (caller-provided): anchor")
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
	text := toolProtocolPrompt("hello", nil, nil, true)
	if !strings.Contains(text, "/mnt/data") {
		t.Fatalf("mcp-gateway-only branch must emit the Windows execution guard: %s", text)
	}
}

func TestToolProtocolPromptNoToolsNoPluginsOmitsGuard(t *testing.T) {
	// Plain chat with no execution capability must not be primed as an agent.
	text := toolProtocolPrompt("hello", nil, nil, false)
	if strings.Contains(text, "/mnt/data") {
		t.Fatalf("plain chat must not carry the execution guard: %s", text)
	}
}

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

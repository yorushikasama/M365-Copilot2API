package web

import (
	"strings"
	"testing"
)

func TestExecutionAnchorForPromptWindowsPath(t *testing.T) {
	prompt := "[system]\nPrimary working directory: D:\\NetPeek\nPlatform: win32\nShell: Git Bash\n\n[user]\nlist the files"
	anchor := executionAnchorForPrompt(prompt)
	if anchor == "" {
		t.Fatal("expected anchor for Windows path evidence")
	}
	for _, want := range []string{"caller-provided", "D:\\NetPeek", "/mnt/data", "Windows machine"} {
		if !strings.Contains(anchor, want) {
			t.Fatalf("anchor missing %q: %s", want, anchor)
		}
	}
}

func TestExecutionAnchorForPromptWindowsKeywords(t *testing.T) {
	prompt := "[user]\nuse the bash tool (Windows PowerShell 5.1) to check"
	if anchor := executionAnchorForPrompt(prompt); anchor == "" {
		t.Fatal("expected anchor for Windows keyword evidence")
	}
}

func TestExecutionAnchorForPromptNoEvidence(t *testing.T) {
	prompt := "[user]\nWhat is the capital of France?"
	if anchor := executionAnchorForPrompt(prompt); anchor != "" {
		t.Fatalf("unexpected anchor for plain chat: %q", anchor)
	}
}

func TestExecutionAnchorNotHardcodedToNetPeek(t *testing.T) {
	// A workspace at a different path must be honored, never replaced by a
	// hardcoded repo path.
	prompt := "[system]\nPrimary working directory: C:\\Code\\other-repo\nPlatform: win32"
	anchor := executionAnchorForPrompt(prompt)
	if anchor == "" || strings.Contains(anchor, "NetPeek") {
		t.Fatalf("anchor must honor caller path, got: %q", anchor)
	}
	if !strings.Contains(anchor, "C:\\Code\\other-repo") {
		t.Fatalf("anchor missing caller path: %q", anchor)
	}
}

func TestAppendExecutionAnchorIdempotent(t *testing.T) {
	anchor := executionAnchorForPrompt("[system]\nPlatform: win32\nWindows")
	prompt := "base"
	first := appendExecutionAnchor(prompt, anchor)
	second := appendExecutionAnchor(first, anchor)
	if first != second {
		t.Fatalf("anchor appended twice:\nfirst=%s\nsecond=%s", first, second)
	}
}

func TestAppendExecutionAnchorEmptyNoop(t *testing.T) {
	if got := appendExecutionAnchor("base", ""); got != "base" {
		t.Fatalf("empty anchor must not change prompt: %q", got)
	}
}

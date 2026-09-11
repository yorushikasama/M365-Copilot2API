package web

import (
	"strings"
	"testing"
)

const (
	citeOpenLit  = "\ue200cite"
	citeSepLit   = "\ue202"
	citeCloseLit = "\ue201"
)

// The upstream wraps web citations in private-use runes (U+E200/E201/E202).
// Stripping them used to live only inside the identity sanitizer, which is
// gated behind M365_PUBLIC_IDENTITY_POLICY and is off in production, so real
// /v1/chat/completions and /v1/responses answers shipped raw marker runes to
// clients. Removal must happen regardless of that flag.
func TestCitationMarkersStrippedWithIdentityPolicyDisabled(t *testing.T) {
	t.Setenv("M365_PUBLIC_IDENTITY_POLICY", "")
	dirty := "北京今天晴，气温 29°C。" + citeOpenLit + citeSepLit + "turn1search3" + citeSepLit + "turn1search6" + citeCloseLit
	got := sanitizePublicAssistantText(dirty)
	if strings.ContainsRune(got, citationMarkerOpen) || strings.ContainsRune(got, citationMarkerClose) {
		t.Fatalf("marker runes survived: %q", got)
	}
	if want := "北京今天晴，气温 29°C。"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestCitationMarkersStrippedWithIdentityPolicyEnabled(t *testing.T) {
	t.Setenv("M365_PUBLIC_IDENTITY_POLICY", "1")
	dirty := "Sunny today." + citeOpenLit + citeSepLit + "turn1news2" + citeCloseLit
	got := sanitizePublicAssistantText(dirty)
	if strings.ContainsRune(got, citationMarkerOpen) {
		t.Fatalf("marker runes survived with policy on: %q", got)
	}
}

// An unenumerated turn kind must still be removed: the upstream keeps adding
// new ones, and the specific pattern only lists search/news/image.
func TestCitationStripHandlesUnknownTurnKind(t *testing.T) {
	t.Setenv("M365_PUBLIC_IDENTITY_POLICY", "")
	dirty := "answer" + citeOpenLit + citeSepLit + "turn9video4" + citeCloseLit + " tail"
	got := sanitizePublicAssistantText(dirty)
	if strings.ContainsRune(got, citationMarkerOpen) || strings.ContainsRune(got, citationMarkerClose) {
		t.Fatalf("unknown turn kind survived: %q", got)
	}
	if got != "answer tail" {
		t.Fatalf("got %q", got)
	}
}

func TestCitationStripHandlesHTMLCiteForm(t *testing.T) {
	t.Setenv("M365_PUBLIC_IDENTITY_POLICY", "")
	got := sanitizePublicAssistantText("answer<cite>turn1search3</cite> tail")
	if strings.Contains(got, "<cite>") {
		t.Fatalf("html cite survived: %q", got)
	}
}

// A marker split across two SSE chunks must be held back and stripped as one
// unit; emitting the first half raw is what a naive per-chunk strip would do.
func TestStreamFilterStripsCitationSplitAcrossChunks(t *testing.T) {
	t.Setenv("M365_PUBLIC_IDENTITY_POLICY", "")
	full := "北京晴。" + citeOpenLit + citeSepLit + "turn1search3" + citeCloseLit + "气温 29°C。"
	for cut := 1; cut < len(full); cut++ {
		if !isRuneBoundary(full, cut) {
			continue
		}
		f := newPublicIdentityStreamFilter("gpt-5.4")
		var got strings.Builder
		got.WriteString(f.Push(full[:cut]))
		got.WriteString(f.Push(full[cut:]))
		got.WriteString(f.Flush())
		out := got.String()
		if strings.ContainsRune(out, citationMarkerOpen) || strings.ContainsRune(out, citationMarkerClose) {
			t.Fatalf("cut=%d leaked markers: %q", cut, out)
		}
		if want := "北京晴。气温 29°C。"; out != want {
			t.Fatalf("cut=%d got %q, want %q", cut, out, want)
		}
	}
}

// An unterminated marker must not stall the stream forever: past the holdback
// cap the pending text is released even though no close rune ever arrives.
func TestStreamFilterDoesNotStallOnUnterminatedMarker(t *testing.T) {
	t.Setenv("M365_PUBLIC_IDENTITY_POLICY", "")
	f := newPublicIdentityStreamFilter("gpt-5.4")
	var got strings.Builder
	got.WriteString(f.Push("head" + citeOpenLit))
	got.WriteString(f.Push(strings.Repeat("x", maxPendingCitationBytes+32)))
	if got.Len() == 0 {
		t.Fatal("filter withheld everything on an unterminated marker")
	}
	got.WriteString(f.Flush())
	if !strings.HasPrefix(got.String(), "head") {
		t.Fatalf("leading text lost: %q", got.String())
	}
}

func TestCitationStripLeavesCleanTextUntouched(t *testing.T) {
	t.Setenv("M365_PUBLIC_IDENTITY_POLICY", "")
	clean := "验收闭环与定向语法检查已完成。No markers here."
	if got := sanitizePublicAssistantText(clean); got != clean {
		t.Fatalf("clean text was modified: %q", got)
	}
}

func isRuneBoundary(s string, i int) bool {
	if i <= 0 || i >= len(s) {
		return true
	}
	return s[i]&0xC0 != 0x80
}

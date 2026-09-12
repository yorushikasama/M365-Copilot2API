package web

import "testing"

// The streaming answer path pushes deltas through publicIdentityStreamFilter,
// which withholds everything after the last sentence boundary and, with the
// identity policy on, emits nothing at all until it holds 1KB. Until 2026-09-12
// the live /v1/chat/completions branch never called Flush, so a short answer
// with no terminal punctuation was delivered as an empty message.
func TestPublicIdentityStreamFilterNeedsFlushToEmitShortAnswer(t *testing.T) {
	t.Setenv("M365_PUBLIC_IDENTITY_POLICY", "1")

	f := newPublicIdentityStreamFilter("m365-copilot")
	var streamed string
	for _, delta := range []string{"好", "的"} {
		streamed += f.Push(delta)
	}
	if streamed != "" {
		t.Fatalf("filter emitted %q before flush; the withheld-tail premise of this test no longer holds", streamed)
	}
	if tail := f.Flush(); tail != "好的" {
		t.Fatalf("flush returned %q, want the full withheld answer", tail)
	}
}

// Same contract for the reasoning channel: consume() holds everything back
// below its 4096-byte threshold, so reasoning_content only reaches the client
// if the stream path flushes at the end of the turn.
func TestPublicReasoningStreamFilterNeedsFlushToEmitShortTrace(t *testing.T) {
	t.Setenv("M365_PUBLIC_IDENTITY_POLICY", "1")

	f := newPublicReasoningStreamFilter()
	if out := f.Push("weighing"); out != "" {
		t.Fatalf("reasoning filter emitted %q before flush", out)
	}
	if tail := f.Flush(); tail != "weighing" {
		t.Fatalf("flush returned %q, want the withheld reasoning", tail)
	}
}

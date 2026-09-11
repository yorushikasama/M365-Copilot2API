package web

import (
	"strings"
	"testing"
)

// feedGate pushes s through the gate in chunks of size n, returning everything
// the content channel would have received (including the final Flush).
func feedGate(g *answerProtocolGate, s string, n int) (string, []string) {
	var out strings.Builder
	var pieces []string
	for i := 0; i < len(s); i += n {
		end := i + n
		if end > len(s) {
			end = len(s)
		}
		if p := g.Push(s[i:end]); p != "" {
			out.WriteString(p)
			pieces = append(pieces, p)
		}
	}
	if p := g.Flush(); p != "" {
		out.WriteString(p)
		pieces = append(pieces, p)
	}
	return out.String(), pieces
}

// TestAnswerGateSuppressesProtocolCallReplayedFromProduction replays the exact
// 2026-09-11 production failure: the upstream streamed a CALL_TOOL answer in
// 4-byte deltas, the opening fragment "CALL" was classified as prose, and the
// caller received the literal protocol line as content on top of the tool call.
func TestAnswerGateSuppressesProtocolCallReplayedFromProduction(t *testing.T) {
	call := `CALL_TOOL: Skill({"skill":"impeccable","args":"实现 NetPeek 历史页已审计出的未落地内容"})`
	g := newAnswerProtocolGate(true)
	got, pieces := feedGate(g, call, 4)
	if got != "" {
		t.Fatalf("protocol call leaked to the content channel: %q (pieces=%q)", got, pieces)
	}
	if !g.Locked() {
		t.Fatal("gate did not lock on a protocol call")
	}
	if !g.LockedNow() {
		t.Fatal("LockedNow must report the transition once")
	}
	if g.LockedNow() {
		t.Fatal("LockedNow must clear after being read")
	}
}

// TestAnswerGatePassesProseThroughUnchanged pins the other direction: a prose
// answer must arrive byte-identical, no matter how it is chunked.
func TestAnswerGatePassesProseThroughUnchanged(t *testing.T) {
	answers := []string{
		"好的，我来实现这个功能。\n\n第一步：读取 docs/UI生成提示词.md。",
		"Sure — here is the plan:\n1. read the file\n2. patch the styles",
		"The result is {\"x\":1} and more prose follows after the object.",
		"短",
		"{",
		"",
	}
	for _, chunk := range []int{1, 3, 4, 7, 64, 4096} {
		for _, want := range answers {
			got, _ := feedGate(newAnswerProtocolGate(true), want, chunk)
			if got != want {
				t.Fatalf("chunk=%d answer=%q: content channel got %q", chunk, want, got)
			}
		}
	}
}

// TestAnswerGateSuppressesMarkerAfterProse covers a model that prepends prose
// and then emits the protocol envelope: the prose is real content, the protocol
// syntax is not.
func TestAnswerGateSuppressesMarkerAfterProse(t *testing.T) {
	text := `Here are the remaining steps.` + "\n" + `{"calls":[{"name":"Read","arguments":{"file_path":"a.md"}}]}`
	for _, chunk := range []int{1, 4, 16, 512} {
		g := newAnswerProtocolGate(true)
		got, _ := feedGate(g, text, chunk)
		if strings.Contains(got, "calls") || strings.Contains(got, "{") {
			t.Fatalf("chunk=%d envelope leaked: %q", chunk, got)
		}
		if !g.Locked() {
			t.Fatalf("chunk=%d gate did not lock", chunk)
		}
		if !strings.HasPrefix(got, "Here are the remaining steps.") {
			t.Fatalf("chunk=%d prose prefix lost: %q", chunk, got)
		}
	}
}

// TestAnswerGateKeepsEveryByteWhenNeverDecided is the "nothing may be silently
// swallowed" contract: an answer shorter than the initial hold limit whose
// bytes stay a viable marker prefix is still delivered by Flush.
func TestAnswerGateKeepsEveryByteWhenNeverDecided(t *testing.T) {
	for _, want := range []string{"C", "CA", "CALL", "call_tool", "{", "{}", "```json"} {
		g := newAnswerProtocolGate(true)
		got, _ := feedGate(g, want, 1)
		if got != want {
			t.Fatalf("answer %q was swallowed: got %q", want, got)
		}
	}
}

// TestAnswerGateDisarmedIsPassThrough: when no tool protocol is armed the gate
// must not touch a single byte.
func TestAnswerGateDisarmedIsPassThrough(t *testing.T) {
	want := `CALL_TOOL: Skill({"skill":"x"})`
	got, _ := feedGate(newAnswerProtocolGate(false), want, 4)
	if got != want {
		t.Fatalf("disarmed gate altered the stream: got %q", got)
	}
}

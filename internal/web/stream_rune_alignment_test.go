package web

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// marshalDelta reproduces what the SSE writer does to a fragment: every delta is
// marshaled on its own, and encoding/json rewrites bytes that are not valid
// UTF-8 as U+FFFD. A fragment that is only valid once concatenated with its
// neighbour is therefore already corrupted by the time it reaches the client.
func marshalDelta(t *testing.T, fragment string) string {
	t.Helper()
	b, err := json.Marshal(map[string]string{"content": fragment})
	if err != nil {
		t.Fatalf("marshal delta: %v", err)
	}
	var back map[string]string
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal delta: %v", err)
	}
	return back["content"]
}

// TestAnswerGateDoesNotSplitRunesAcrossDeltas replays the 2026-09-11 report.
// The upstream streamed a Chinese answer in small deltas; the gate's rolling
// holdback cut at len(buf)-protocolHoldWindow, a raw byte offset, so the emitted
// piece ended mid-rune and the next one began mid-rune. Each half was marshaled
// separately, so every straddled CJK character reached the client as three
// replacement characters: "当\ufffd\ufffd\ufffd代码".
func TestAnswerGateDoesNotSplitRunesAcrossDeltas(t *testing.T) {
	answer := "当前代码已重新核对，历史页这批实现已经存在，静态检查与完整 Tauri 生产构建均通过，没有发现新的阻断错误。"

	// Sweep the delta sizes: the bug only shows when a rune straddles the cut,
	// which depends on how the byte stream lines up with the 16-byte window.
	for _, deltaSize := range []int{1, 2, 3, 4, 5, 7, 8, 13, 16, 17, 32} {
		g := newAnswerProtocolGate(true)
		var rebuilt strings.Builder
		for i := 0; i < len(answer); i += deltaSize {
			end := i + deltaSize
			if end > len(answer) {
				end = len(answer)
			}
			piece := g.Push(answer[i:end])
			if piece == "" {
				continue
			}
			if !utf8.ValidString(piece) {
				t.Fatalf("delta=%d: gate emitted invalid UTF-8: %q", deltaSize, piece)
			}
			rebuilt.WriteString(marshalDelta(t, piece))
		}
		if tail := g.Flush(); tail != "" {
			if !utf8.ValidString(tail) {
				t.Fatalf("delta=%d: flushed tail is invalid UTF-8: %q", deltaSize, tail)
			}
			rebuilt.WriteString(marshalDelta(t, tail))
		}
		if got := rebuilt.String(); got != answer {
			t.Fatalf("delta=%d: answer was garbled\n got: %q\nwant: %q", deltaSize, got, answer)
		}
	}
}

// TestAnswerGateStillSuppressesProtocolAfterRuneAlignment guards the fix from
// regressing the gate's actual job: retreating to a rune boundary must not let
// a protocol marker slip out.
func TestAnswerGateStillSuppressesProtocolAfterRuneAlignment(t *testing.T) {
	call := `CALL_TOOL: Skill({"skill":"impeccable","args":"实现历史页"})`
	for _, deltaSize := range []int{1, 3, 4, 8, 16} {
		g := newAnswerProtocolGate(true)
		var out strings.Builder
		for i := 0; i < len(call); i += deltaSize {
			end := i + deltaSize
			if end > len(call) {
				end = len(call)
			}
			out.WriteString(g.Push(call[i:end]))
		}
		out.WriteString(g.Flush())
		if got := out.String(); got != "" {
			t.Fatalf("delta=%d: protocol call leaked as content: %q", deltaSize, got)
		}
	}
}

// TestAnswerGateEmitsProseBeforeAMarkerIntact covers the mixed case: prose that
// precedes a marker must arrive complete and undamaged.
func TestAnswerGateEmitsProseBeforeAMarkerIntact(t *testing.T) {
	prose := "好的，我先读取历史页的实现，然后再动手。"
	full := prose + `CALL_TOOL: Read({"path":"a"})`
	g := newAnswerProtocolGate(true)
	var out strings.Builder
	for i := 0; i < len(full); i += 3 {
		end := i + 3
		if end > len(full) {
			end = len(full)
		}
		piece := g.Push(full[i:end])
		if !utf8.ValidString(piece) {
			t.Fatalf("invalid UTF-8 emitted: %q", piece)
		}
		out.WriteString(piece)
	}
	out.WriteString(g.Flush())
	if got := out.String(); got != prose {
		t.Fatalf("prose before the marker was damaged\n got: %q\nwant: %q", got, prose)
	}
}

// TestReasoningFilterDoesNotSplitRunes covers the same byte-offset cut in the
// reasoning stream filter, which withholds a 256-byte tail once its buffer
// passes 4096 bytes.
func TestReasoningFilterDoesNotSplitRunes(t *testing.T) {
	t.Setenv("M365_PUBLIC_IDENTITY_POLICY", "1")
	// No sentence-ending punctuation, so the boundary path cannot trigger and the
	// byte-count fallback is the one under test. Long enough to pass 4096 bytes.
	reasoning := strings.Repeat("推理中间步骤", 400)

	f := newPublicReasoningStreamFilter()
	var rebuilt strings.Builder
	for i := 0; i < len(reasoning); i += 7 {
		end := i + 7
		if end > len(reasoning) {
			end = len(reasoning)
		}
		piece := f.Push(reasoning[i:end])
		if piece == "" {
			continue
		}
		if !utf8.ValidString(piece) {
			t.Fatalf("reasoning filter emitted invalid UTF-8: %q", piece)
		}
		rebuilt.WriteString(marshalDelta(t, piece))
	}
	if tail := f.Flush(); tail != "" {
		if !utf8.ValidString(tail) {
			t.Fatalf("reasoning flush is invalid UTF-8: %q", tail)
		}
		rebuilt.WriteString(marshalDelta(t, tail))
	}
	if got := rebuilt.String(); got != reasoning {
		t.Fatalf("reasoning was garbled: %d bytes out of %d survived", len(got), len(reasoning))
	}
}

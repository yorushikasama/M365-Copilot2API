package web

import "strings"

// protocolMarkers are the byte sequences that make up the answer turn's textual
// tool protocol (see answerToolProtocol). They are transport instructions and
// must never reach the client's content channel as text.
var protocolMarkers = []string{
	"CALL_TOOL:",
	"call_tool:",
	"CALL_TOOL：",
	"call_tool：",
	`{"calls"`,
}

const (
	// protocolHoldInitial is how many undecided bytes are buffered before the
	// opening classification gives up. A legitimate answer that merely starts
	// with "{" or with a letter that could still become a marker must not be
	// held back forever.
	protocolHoldInitial = 512
	// protocolHoldWindow is the rolling tail withheld from the content channel
	// for the whole stream, so a marker split across two deltas is detected
	// before any of its bytes are emitted.
	protocolHoldWindow = 16
)

// answerProtocolGate keeps the answer turn's textual tool protocol off the
// content channel.
//
// The upstream has no native tool channel, so the answer turn is instructed to
// reply with exactly one `CALL_TOOL: name({...})` line or one
// `{"calls":[...]}` envelope. Those bytes are an instruction to the gateway,
// not an answer for the user. Deciding "is this a tool call?" from the opening
// bytes of the stream alone is not enough, for two reasons:
//
//  1. Stream deltas are tiny — the measured first delta is 4-5 bytes — so the
//     opening fragment is a prefix such as "CALL". A decision taken there said
//     "definitely not a call" and released the holdback, so the caller received
//     the literal protocol line as content while the gateway ALSO emitted the
//     parsed tool call (2026-09-11: `CALL_TOOL: Skill({...})` reached the
//     client). classifyAnswerOutputPrefix is now prefix-aware; this gate holds
//     the bytes until the disposition is actually known.
//  2. A model may prefix the marker with prose, so a marker can appear after
//     the opening bytes were already released.
//
// The gate solves both: it buffers while the buffer is still a viable prefix of
// a marker, and it withholds a short rolling tail for the entire stream so a
// marker straddling two deltas is still seen. Once a marker is observed the
// gate locks: the rest of the turn is dropped instead of streamed, while the
// caller's own parse keeps operating on the full accumulated text.
type answerProtocolGate struct {
	armed          bool
	measured       bool
	locking        bool
	lockedNow      bool
	promissory     bool
	promissoryHeld bool
	window         strings.Builder
}

func newAnswerProtocolGate(armed bool) *answerProtocolGate {
	return &answerProtocolGate{armed: armed}
}

// ArmPromissory makes the gate hold prose that is a bare promise of future work
// instead of an answer, so an execution request answered with "我会把…补齐"
// never reaches the content channel and the caller can retry with a protocol
// correction from a clean slate. Only the caller knows whether the turn was an
// execution request; the gate never decides that on its own.
func (g *answerProtocolGate) ArmPromissory(on bool) { g.promissory = on }

// PromissoryHeld reports whether the gate is (or ended up) withholding prose
// because it read as a bare promise. Checked before Flush so the caller can
// discard the held promise rather than put it on the wire.
func (g *answerProtocolGate) PromissoryHeld() bool { return g.promissoryHeld }

// Push feeds one upstream delta and returns the bytes that may be written to
// the content channel ("" when the gate is withholding them).
func (g *answerProtocolGate) Push(part string) string {
	if !g.armed {
		return part
	}
	if part == "" || g.locking {
		return ""
	}
	g.window.WriteString(part)
	buf := g.window.String()
	if !g.measured {
		decided, isCall := classifyAnswerOutputPrefix(buf)
		if !decided && len(buf) < protocolHoldInitial {
			return ""
		}
		if !isCall && g.promissory {
			if isPromissoryPlan(buf) {
				// A promise of future work is not an answer to an execution
				// request. Hold it until the turn ends; isPromissoryPlan stops
				// matching once the buffer grows past a bare promise, which
				// releases the hold naturally for a real answer.
				g.promissoryHeld = true
				return ""
			}
			if len(buf) < promissoryHoldInitial || isPromissoryPrefix(buf) {
				// Undecided, not a promise: the opening fragment is still
				// growing toward a promise phrase ("我" → "我会"). Deciding
				// here is the prefix-safety bug the protocol marker already
				// paid for once, and a mid-rune delta must not decide either.
				return ""
			}
		}
		g.promissoryHeld = false
		g.measured = true
		if isCall {
			g.lock()
			return ""
		}
		// Not a call: fall through to the marker scan below, because the very
		// same buffer may already contain the protocol syntax further in (a
		// model that prefixes the envelope with prose, or a 512-byte first
		// delta that carries the whole answer).
	}
	if i := protocolMarkerIndex(buf); i >= 0 {
		out := buf[:i]
		g.lock()
		return out
	}
	if len(buf) <= protocolHoldWindow {
		return ""
	}
	cut := len(buf) - protocolHoldWindow
	out := buf[:cut]
	g.window.Reset()
	g.window.WriteString(buf[cut:])
	return out
}

// Flush releases the withheld tail. It returns "" once the gate has locked: the
// withheld bytes are protocol syntax, the caller parses the full accumulated
// text separately, and re-emitting them would only re-leak the marker.
func (g *answerProtocolGate) Flush() string {
	if !g.armed || g.locking {
		g.window.Reset()
		return ""
	}
	out := g.window.String()
	g.window.Reset()
	return out
}

// LockedNow reports (and clears) the transition into the locked state, so the
// caller can log the suppression exactly once.
func (g *answerProtocolGate) LockedNow() bool {
	now := g.lockedNow
	g.lockedNow = false
	return now
}

// Locked reports whether a protocol marker was observed.
func (g *answerProtocolGate) Locked() bool { return g.locking }

func (g *answerProtocolGate) lock() {
	g.locking = true
	g.lockedNow = true
	g.window.Reset()
}

// protocolMarkerIndex returns the offset of the earliest protocol marker in s,
// or -1 when there is none.
func protocolMarkerIndex(s string) int {
	best := -1
	for _, m := range protocolMarkers {
		if i := strings.Index(s, m); i >= 0 && (best < 0 || i < best) {
			best = i
		}
	}
	return best
}

// isProtocolMarkerPrefix reports whether s could still grow into a protocol
// marker once more deltas are appended.
func isProtocolMarkerPrefix(s string) bool {
	return strings.HasPrefix("call_tool:", s) || strings.HasPrefix("call_tool：", s)
}

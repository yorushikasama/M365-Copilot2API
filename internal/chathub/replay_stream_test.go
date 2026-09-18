package chathub

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// deltaTrace records each delta the client hands the caller, so a test can see
// what a streaming client actually received and when.
type deltaTrace struct {
	mu     sync.Mutex
	text   strings.Builder
	stamps []time.Time
}

func (d *deltaTrace) emit(s string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.text.WriteString(s)
	d.stamps = append(d.stamps, time.Now())
	return nil
}

func (d *deltaTrace) String() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.text.String()
}

func (d *deltaTrace) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.stamps)
}

// worstGap is the longest interval between consecutive deltas. It is used by
// the resume test to show the stream did not stall across the rewrite.
func (d *deltaTrace) worstGap() time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	var worst time.Duration
	for i := 1; i < len(d.stamps); i++ {
		if g := d.stamps[i].Sub(d.stamps[i-1]); g > worst {
			worst = g
		}
	}
	return worst
}

func testAccount() Account {
	return Account{OID: "00000000-0000-0000-0000-000000000001", TID: "00000000-0000-0000-0000-000000000002", AccessToken: "test-token"}
}

// runFakeStream drives one ChatWithDelta against a scripted fake upstream and
// returns the result text plus what the caller received.
func runFakeStream(t *testing.T, frames func(conn *websocket.Conn)) (string, *deltaTrace) {
	t.Helper()
	newFakeChathub(t, frames)
	tr := &deltaTrace{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := NewClient().ChatWithDelta(ctx, testAccount(), Request{Text: "hello", SessionID: "s1", ConversationID: "c1"}, tr.emit)
	if err != nil {
		t.Fatalf("ChatWithDelta: %v", err)
	}
	return res.Text, tr
}

// script performs the handshake, sends frames after the client's payload
// arrives, then waits for the client to consume them before returning.
func script(c *websocket.Conn, gap time.Duration, frames ...string) {
	if !startStream(c, 3*time.Second) {
		return
	}
	stop := make(chan struct{})
	go drainReads(c, stop)
	defer close(stop)
	sendScript(c, gap, frames...)
	time.Sleep(300 * time.Millisecond)
}

// The documented steady state (docs/har-mining/05-streaming-frames.md §3.2):
// writeAtCursor deltas append, and each cumulative snapshot is a superset that
// arrives *after* the deltas it contains. The client must emit each fragment
// exactly once, never doubling a section.
func TestReplayRedundantChannelsDoNotDuplicate(t *testing.T) {
	segs := []string{"# 标题\n\n", "第一段内容。", "第二段内容。", "第三段内容。"}
	var full strings.Builder
	var frames []string
	// First text frame is the cursor anchor carrying the first snapshot.
	full.WriteString(segs[0])
	frames = append(frames, snapshotFrame(full.String()))
	for _, s := range segs[1:] {
		// A delta appends, then a cumulative snapshot confirms it.
		frames = append(frames, cursorDeltaFrame(s))
		full.WriteString(s)
		frames = append(frames, snapshotFrame(full.String()))
	}
	final := full.String()
	frames = append(frames, resultFrame(final), completionFrame())

	text, tr := runFakeStream(t, func(c *websocket.Conn) { script(c, 10*time.Millisecond, frames...) })
	if text != final {
		t.Fatalf("result = %q, want %q", text, final)
	}
	if got := tr.String(); got != final {
		t.Fatalf("client received %q, want %q", got, final)
	}
	for _, s := range segs {
		if n := strings.Count(tr.String(), s); n != 1 {
			t.Fatalf("segment %q appeared %d times, want 1 (redundant channels were concatenated)", s, n)
		}
	}
}

// The production-dominant divergence shape (94 of 112 logged events): a
// cumulative snapshot that lags behind the deltas already streamed, i.e. the
// snapshot is a prefix of what the client has. It carries no new content and
// must be ignored, not treated as a rewrite that silences the turn.
func TestReplayLaggingSnapshotIsIgnored(t *testing.T) {
	head := "已经流式输出的正文。"
	ahead := head + "游标已经追加了更多内容，"
	final := ahead + "最终收尾。"
	text, tr := runFakeStream(t, func(c *websocket.Conn) {
		script(c, 10*time.Millisecond,
			snapshotFrame(head),
			cursorDeltaFrame("游标已经追加了更多内容，"),
			// Lagging cumulative snapshot: a prefix of what was streamed.
			snapshotFrame(head),
			snapshotFrame(head+"游标已经"),
			resultFrame(final), completionFrame(),
		)
	})
	if text != final {
		t.Fatalf("result = %q, want %q", text, final)
	}
	if got := tr.String(); got != final {
		t.Fatalf("client received %q, want %q", got, final)
	}
}

// The 2026-09-10 class: upstream regenerates the answer, so the new snapshot
// shares almost nothing with what was streamed. Splicing would fabricate a
// hybrid, so the stream stops after the stale fragment and the authoritative
// final is delivered whole.
//
// Bytes already on the wire cannot be retracted, so the caller necessarily sees
// the stale fragment first. What must hold is that the answer arrives complete
// and that no *hybrid* is synthesised — the stale text must not be interleaved
// into the new answer.
func TestReplayRegenerationSuppressesThenDeliversFinal(t *testing.T) {
	stale := "这是被上游废弃的旧版本开头，已经发出去了，内容相当长以便拉开与最终答案的距离。"
	final := "这是一份全新的答案，与旧版本没有公共前缀，应当完整送达给客户端，不能与旧内容拼接。"
	text, tr := runFakeStream(t, func(c *websocket.Conn) {
		script(c, 10*time.Millisecond,
			snapshotFrame(stale),
			snapshotFrame(final),
			resultFrame(final), completionFrame(),
		)
	})
	if text != final {
		t.Fatalf("result = %q, want the authoritative final %q", text, final)
	}
	got := tr.String()
	// The stale fragment must appear as its own prefix, never spliced into the
	// middle of the new answer.
	if i := strings.Index(got, stale); i != 0 {
		t.Fatalf("stale fragment appears at offset %d, want 0: %q", i, got)
	}
	if strings.Contains(got[len(stale):], stale) {
		t.Fatalf("stale fragment was spliced into the regenerated answer: %q", got)
	}
	// The remainder of the final must be delivered. The re-emit resumes at the
	// common prefix, so the delivered tail is final[lcp:], not the whole final.
	rest := final[commonPrefixLen(stale, final):]
	if rest == "" || !strings.HasSuffix(got, rest) {
		t.Fatalf("client stream %q does not end with the final's remainder %q", got, rest)
	}
	if !strings.Contains(got, rest) {
		t.Fatalf("client stream %q never delivered the regenerated answer", got)
	}
}

// A bounded tail correction (the 2026-09-11 case) must resume rather than
// silence: content keeps flowing before the turn ends. Completion is gated on
// the test, so if the stream went silent nothing would arrive in the window.
func TestReplayBoundedCorrectionResumesBeforeCompletion(t *testing.T) {
	head := strings.Repeat("已经输出的正文内容。", 8)
	stale := head + "旧尾巴"
	fixed := head + "新尾巴"
	final := fixed + "，然后继续输出直到结束。"
	if strings.HasPrefix(fixed, stale) || commonPrefixLen(stale, fixed) == 0 {
		t.Fatal("fixture must be a genuine bounded rewrite")
	}

	released := make(chan struct{})
	diverged := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(released) }) }

	newFakeChathub(t, func(c *websocket.Conn) {
		if !startStream(c, 3*time.Second) {
			return
		}
		stop := make(chan struct{})
		go drainReads(c, stop)
		defer close(stop)
		_ = c.WriteMessage(websocket.TextMessage, []byte(snapshotFrame(stale)))
		time.Sleep(60 * time.Millisecond)
		// The divergent frame: upstream rewrites the tail.
		_ = c.WriteMessage(websocket.TextMessage, []byte(snapshotFrame(fixed)))
		close(diverged)
		// Hold completion until the test has inspected the stream.
		select {
		case <-released:
		case <-time.After(5 * time.Second):
		}
		sendScript(c, 10*time.Millisecond, resultFrame(final), completionFrame())
	})

	tr := &deltaTrace{}
	done := make(chan struct{})
	var res Result
	var err error
	go func() {
		defer close(done)
		res, err = NewClient().ChatWithDelta(context.Background(), testAccount(), Request{Text: "hi", SessionID: "s", ConversationID: "c"}, tr.emit)
	}()

	<-diverged
	// Completion is still held. If the stream resumed, the corrected tail must
	// already have reached the caller.
	resumed := false
	deadline := time.After(2 * time.Second)
	for !resumed {
		if strings.Contains(tr.String(), "新尾巴") {
			resumed = true
			break
		}
		select {
		case <-deadline:
			goto done
		case <-time.After(20 * time.Millisecond):
		}
	}
done:
	release()
	<-done
	if !resumed {
		t.Fatalf("nothing arrived before completion; stream was silenced (deltas=%d, text=%q)", tr.count(), tr.String())
	}
	if err != nil {
		t.Fatalf("ChatWithDelta: %v", err)
	}
	if res.Text != final {
		t.Fatalf("result = %q, want %q", res.Text, final)
	}
	// Resuming is about keeping the stream moving: the rewrite must not have
	// opened a stall wide enough for a client to notice. The frames are 60ms
	// apart, so anything near a second means the turn went quiet.
	if gap := tr.worstGap(); gap > 500*time.Millisecond {
		t.Fatalf("stream stalled for %v across the rewrite (deltas=%d)", gap, tr.count())
	}
}

// soakScenario is one scripted turn plus the invariants that must hold for it.
type soakScenario struct {
	frames []string
	// want is the authoritative final text the turn must resolve to.
	want string
	// tail is the suffix the client's stream must end with. A rewrite leaves the
	// invalidated bytes on the wire (they cannot be retracted), so the stream is
	// not necessarily equal to want — but it must always finish with the part of
	// want that had not already been delivered.
	tail string
	// stale and newer are the texts bracketing a divergence, used to assert the
	// classification decision directly.
	stale, newer string
	action       snapshotAction
}

// tailOf is the part of want past the longest prefix the client already holds.
func tailOf(already, want string) string {
	return want[commonPrefixLen(already, want):]
}

// Multi-round soak over every incident class, several rounds each. Three
// invariants hold for every class:
//
//   - the turn result is always the authoritative final text;
//   - the client's stream always ends with the part of that text it had not
//     already received (the stale bytes a rewrite invalidates are already on
//     the wire and cannot be retracted, so they may precede it);
//   - the divergent frame is classified as the class requires — a bounded tail
//     correction resumes, a wholesale regeneration suppresses.
//
// Whether the stream keeps flowing during a rewrite is checked directly, before
// completion, by TestReplayBoundedCorrectionResumesBeforeCompletion.
func TestReplayMultiRoundSoak(t *testing.T) {
	head := strings.Repeat("正文片段，", 12)
	scenarios := []struct {
		name  string
		build func(round int) soakScenario
	}{
		{
			name: "clean stream",
			build: func(r int) soakScenario {
				a := fmt.Sprintf("第%d轮开始。", r)
				delta := "这是完整答案，正常结束。"
				f := a + delta
				return soakScenario{
					frames: []string{snapshotFrame(a), cursorDeltaFrame(delta), resultFrame(f), completionFrame()},
					want:   f, tail: f, stale: a, newer: f, action: snapshotAppend,
				}
			},
		},
		{
			name: "lagging snapshot",
			build: func(r int) soakScenario {
				a := fmt.Sprintf("第%d轮正文，", r) + head
				f := a + "补充收尾。"
				return soakScenario{
					// The trailing snapshot is a prefix of what was streamed.
					frames: []string{snapshotFrame(a), cursorDeltaFrame("补充收尾。"), snapshotFrame(a), resultFrame(f), completionFrame()},
					want:   f, tail: f, stale: f, newer: a, action: snapshotIgnore,
				}
			},
		},
		{
			name: "bounded tail correction",
			build: func(r int) soakScenario {
				stale := head + "旧尾巴"
				fixed := head + "新尾巴"
				f := fixed + "结束。"
				return soakScenario{
					frames: []string{snapshotFrame(stale), snapshotFrame(fixed), resultFrame(f), completionFrame()},
					want:   f, tail: tailOf(stale, f), stale: stale, newer: fixed, action: snapshotResume,
				}
			},
		},
		{
			name: "wholesale regeneration",
			build: func(r int) soakScenario {
				stale := fmt.Sprintf("第%d轮被废弃的旧答案，写得很长以便与最终答案没有公共前缀。", r)
				f := fmt.Sprintf("第%d轮全新的答案，完整送达，绝不与旧内容拼接。", r)
				return soakScenario{
					frames: []string{snapshotFrame(stale), snapshotFrame(f), resultFrame(f), completionFrame()},
					want:   f, tail: tailOf(stale, f), stale: stale, newer: f, action: snapshotSuppress,
				}
			},
		},
	}

	const rounds = 4
	for round := 1; round <= rounds; round++ {
		for _, sc := range scenarios {
			sc := sc
			t.Run(fmt.Sprintf("%s/round%d", sc.name, round), func(t *testing.T) {
				s := sc.build(round)
				text, tr := runFakeStream(t, func(c *websocket.Conn) { script(c, 10*time.Millisecond, s.frames...) })
				if text != s.want {
					t.Errorf("result = %q, want %q", text, s.want)
				}
				if got := tr.String(); !strings.HasSuffix(got, s.tail) {
					t.Errorf("client stream %q does not end with %q", got, s.tail)
				}
				if a, _ := classifySnapshot(s.stale, s.newer); a != s.action {
					t.Errorf("classifySnapshot(%q, %q) = %v, want %v", s.stale, s.newer, a, s.action)
				}
			})
		}
	}
}

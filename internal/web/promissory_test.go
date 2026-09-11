package web

import (
	"strings"
	"testing"
)

// TestIsPromissoryPlan pins the third failure shape: an execution request
// answered with a promise of future work instead of a tool call. The guard must
// catch bare promises and must not touch real answers or work reports.
func TestIsPromissoryPlan(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{
			"live 2026-09-11 promise",
			"我会把缺失的日期逻辑、表单状态、响应式浮岛、图表高度和历史检查栏样式一起补齐，然后用定向语法检查、构建和多尺寸页面验收闭环。",
			true,
		},
		{"english promise", "I will update the styles and then run the build.", true},
		{"english contraction", "I'll patch the history chart next.", true},
		{"let me", "Let me first read the file, then patch it.", true},
		{"markdown bullet promise", "* 我会先补 dayKeysBetween，再稳定图表高度。", true},
		{"heading promise", "## 接下来我会补齐响应式浮岛", true},
		{"with code", "我会这样改：\n```go\nfunc x() {}\n```", false},
		{"reports work", "已完成 dayKeysBetween 的补齐，并跑了定向构建。", false},
		{"promise plus evidence", "我会补齐这些，已完成其中两项。", false},
		{"plain answer", "这个函数用哈希表做 O(1) 查找。", false},
		{
			"too long to be a bare promise",
			"我会" + strings.Repeat("补齐这些内容，", 60) + "然后验收。",
			false,
		},
		{"empty", "", false},
	}
	for _, tc := range cases {
		if got := isPromissoryPlan(tc.text); got != tc.want {
			t.Fatalf("%s: isPromissoryPlan(%q) = %v, want %v", tc.name, tc.text, got, tc.want)
		}
	}
}

// TestAnswerGateHoldsPromissoryPlanReplayedFromProduction replays the
// 2026-09-11 failure: an execution request answered with a plan of what the
// model was about to do. With the hold armed the promise never reaches the
// content channel, so the caller can retry from a clean slate.
func TestAnswerGateHoldsPromissoryPlanReplayedFromProduction(t *testing.T) {
	promise := "我会把缺失的日期逻辑、表单状态、响应式浮岛、图表高度和历史检查栏样式一起补齐，然后用定向语法检查、构建和多尺寸页面验收闭环。"
	for _, chunk := range []int{4, 16, 64, 4096} {
		g := newAnswerProtocolGate(true)
		g.ArmPromissory(true)
		var pieces []string
		for i := 0; i < len(promise); i += chunk {
			end := i + chunk
			if end > len(promise) {
				end = len(promise)
			}
			if p := g.Push(promise[i:end]); p != "" {
				pieces = append(pieces, p)
			}
		}
		if len(pieces) != 0 {
			t.Fatalf("chunk=%d promise reached the content channel: %q", chunk, pieces)
		}
		if !g.PromissoryHeld() {
			t.Fatalf("chunk=%d gate did not record the promissory hold", chunk)
		}
		if held := g.Flush(); held != promise {
			t.Fatalf("chunk=%d Flush must return the held promise for the caller, got %q", chunk, held)
		}
	}
}

// TestAnswerGatePromissoryHoldReleasesRealAnswers is the "never swallow an
// answer" contract for the new hold: prose that merely opens with a promise
// phrase and then grows into a real answer streams unchanged.
func TestAnswerGatePromissoryHoldReleasesRealAnswers(t *testing.T) {
	long := "我会从三个方面说明这个设计。" + strings.Repeat("这一段是对方案的具体说明，包含结论和证据。", 40)
	for _, chunk := range []int{4, 64, 512} {
		g := newAnswerProtocolGate(true)
		g.ArmPromissory(true)
		got, _ := feedGate(g, long, chunk)
		if got != long {
			t.Fatalf("chunk=%d a real answer was withheld: got %d bytes, want %d", chunk, len(got), len(long))
		}
		if g.PromissoryHeld() {
			t.Fatalf("chunk=%d gate must release the hold once the text is no longer a bare promise", chunk)
		}
	}
}

// TestAnswerGatePromissoryDisarmedPassesPromises: a conversational turn never
// arms the hold, so "我会建议…" answers are untouched.
func TestAnswerGatePromissoryDisarmedPassesPromises(t *testing.T) {
	promise := "我会建议先补 dayKeysBetween，再看图表高度。"
	for _, chunk := range []int{4, 16, 4096} {
		got, _ := feedGate(newAnswerProtocolGate(true), promise, chunk)
		if got != promise {
			t.Fatalf("chunk=%d unarmed gate altered the promise: got %q", chunk, got)
		}
	}
}

// TestAnswerGatePromissoryHoldDoesNotMaskProtocolCalls: the promise hold must
// not shadow the protocol gate — a CALL_TOOL answer still locks instead of
// being reported as a held promise.
func TestAnswerGatePromissoryHoldDoesNotMaskProtocolCalls(t *testing.T) {
	call := `CALL_TOOL: Bash({"command":"ls"})`
	g := newAnswerProtocolGate(true)
	g.ArmPromissory(true)
	got, _ := feedGate(g, call, 4)
	if got != "" {
		t.Fatalf("protocol call leaked while promissory hold was armed: %q", got)
	}
	if !g.Locked() {
		t.Fatal("gate did not lock on the protocol call")
	}
	if g.PromissoryHeld() {
		t.Fatal("a protocol call must not be reported as a held promise")
	}
}

// TestIsPromissoryPrefixSurvivesTruncatedUTF8 pins the bug found while writing
// this guard: strings.ToLower substitutes U+FFFD for an incomplete trailing
// rune, which changed the byte prefix and made the 4-byte opening delta of
// "我会…" ("我\xe4") read as "not a promise" — the same prefix-safety trap the
// CALL_TOOL classification fell into on 2026-09-11.
func TestIsPromissoryPrefixSurvivesTruncatedUTF8(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"我", true},
		{"我\xe4", true},
		{"我\xe4\xbc", true},
		{"我会", true},
		{"我很", false},
		{"这个", false},
		{"I", true},
		{"I\xe4", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isPromissoryPrefix(tc.s); got != tc.want {
			t.Fatalf("isPromissoryPrefix(%q) = %v, want %v", tc.s, got, tc.want)
		}
	}
}

// TestUserMessageDemandsActionGatesTheHold documents what arms the hold: only
// an execution request does.
func TestUserMessageDemandsActionGatesTheHold(t *testing.T) {
	request := "<task-notification>\n<status>completed</status>\n<summary>Background command \"Inspect exact history implementation and available verification scripts\" completed (exit code 0)</summary>\n</task-notification>"
	if !userMessageDemandsAction(request) {
		t.Fatal("the 2026-09-11 triggering message must read as an execution request")
	}
	if userMessageDemandsAction("今天天气怎么样？") {
		t.Fatal("a conversational message must not arm the hold")
	}
}

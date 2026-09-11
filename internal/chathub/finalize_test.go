package chathub

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

// collectEmit returns an emit func appending every delta to out.
func collectEmit(out *[]string) func(string) error {
	return func(d string) error {
		*out = append(*out, d)
		return nil
	}
}

func TestCommonPrefixLenNeverCutsUTF8Rune(t *testing.T) {
	// These two runes share their UTF-8 leading byte. A byte-wise prefix is one
	// byte long, but slicing there would emit an invalid continuation byte and
	// surface as the U+FFFD replacement character.
	streamed := "Āx"
	snapshot := "Áy"
	overlap := commonPrefixLen(streamed, snapshot)
	if overlap != 0 {
		t.Fatalf("commonPrefixLen() = %d, want 0; prefix must stop at a rune boundary", overlap)
	}
	if !utf8.ValidString(snapshot[overlap:]) {
		t.Fatalf("suffix %q is not valid UTF-8", snapshot[overlap:])
	}
}

func TestFinalizeTextKeepsStreamedWhenFinalNotLonger(t *testing.T) {
	cases := []struct {
		name     string
		streamed string
		final    string
		want     string
	}{
		{"both empty", "", "", ""},
		{"final empty", "hello", "", "hello"},
		{"equal", "答案在这里", "答案在这里", "答案在这里"},
		{"final shorter", "a longer streamed answer", "short", "a longer streamed answer"},
		{"same length different content", "abcd", "wxyz", "abcd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var emitted []string
			got, err := finalizeText(tc.streamed, tc.final, 0, collectEmit(&emitted))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			if len(emitted) != 0 {
				t.Fatalf("expected no emitted deltas, got %v", emitted)
			}
		})
	}
}

func TestFinalizeTextEmitsMissingTail(t *testing.T) {
	// Regression for issue #51: non-prefix writeAtCursor fragments are
	// skipped during the stream, leaving streamed a stale prefix of the
	// authoritative final message. The missing tail must be delivered to
	// streaming clients and the returned text must be the full answer.
	streamed := "首先，"
	final := "首先，我们需要检查配置文件。"
	var emitted []string
	got, err := finalizeText(streamed, final, 42, collectEmit(&emitted))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != final {
		t.Fatalf("got %q, want %q", got, final)
	}
	if want := []string{"我们需要检查配置文件。"}; len(emitted) != 1 || emitted[0] != want[0] {
		t.Fatalf("emitted %v, want %v", emitted, want)
	}
	// Reassembled stream must be valid UTF-8 and byte-identical to final.
	if joined := streamed + strings.Join(emitted, ""); joined != final {
		t.Fatalf("reassembled %q, want %q", joined, final)
	}
}

func TestFinalizeTextEmitsWholeFinalWhenNothingStreamed(t *testing.T) {
	final := "完整的最终回答"
	var emitted []string
	got, err := finalizeText("", final, 0, collectEmit(&emitted))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != final {
		t.Fatalf("got %q, want %q", got, final)
	}
	if len(emitted) != 1 || emitted[0] != final {
		t.Fatalf("emitted %v, want the whole final message", emitted)
	}
}

func TestFinalizeTextPrefersFinalOnDivergence(t *testing.T) {
	// A poisoned early delta means streamed is not a prefix of final.
	// Already-sent deltas cannot be retracted, so nothing already on the wire
	// is repeated — but the authoritative final message's tail is still
	// delivered, otherwise a streaming caller keeps only the stale fragment.
	streamed := "**"
	final := "好的，这是完整的答案。"
	var emitted []string
	got, err := finalizeText(streamed, final, 57, collectEmit(&emitted))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != final {
		t.Fatalf("got %q, want %q", got, final)
	}
	if len(emitted) != 1 || emitted[0] != final {
		t.Fatalf("expected the final message to be re-emitted on divergence, got %v", emitted)
	}
}

func TestFinalizeTextPrefersFinalAfterMidStreamRewrite(t *testing.T) {
	// Regression for the 2026-09-10 07:33 incident: upstream rewrote streamed
	// content mid-answer (three throttled regenerations), and the glued
	// streamed text grew LONGER than the authoritative final message. The
	// rewrite must win the result back to final — length alone must not
	// keep a Frankenstein hybrid of several generations. Only the part of
	// final the caller has not seen is emitted, so the shared opening is not
	// duplicated on the wire.
	streamed := "clean start\n<File>v1</File>\n具栏空间不足" // glued hybrid, longer than final
	final := "clean start\n## 具体改动方案\n\n- `.hist-chart` 使用 `flex: 1`"
	var emitted []string
	got, err := finalizeText(streamed, final, 3, collectEmit(&emitted))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != final {
		t.Fatalf("got %q, want the authoritative final %q", got, final)
	}
	wantTail := "## 具体改动方案\n\n- `.hist-chart` 使用 `flex: 1`"
	if len(emitted) != 1 || emitted[0] != wantTail {
		t.Fatalf("expected only the unseen tail %q, got %v", wantTail, emitted)
	}
}

func TestFinalizeTextDivergenceReemitCanBeDisabled(t *testing.T) {
	t.Setenv("M365_STREAM_DIVERGENCE_REEMIT", "0")
	// The rewrite must actually diverge — a streamed prefix of final takes the
	// "missing tail" branch and is unaffected by this switch.
	streamed := "我会把缺失的日期逻辑、表单状态、响应式浮岛一起补齐"
	final := "我会把缺失的日期逻辑全部补齐，然后跑定向构建验收。"
	var emitted []string
	got, err := finalizeText(streamed, final, 22, collectEmit(&emitted))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != final {
		t.Fatalf("got %q, want %q", got, final)
	}
	if len(emitted) != 0 {
		t.Fatalf("expected the kill switch to suppress re-emission, got %v", emitted)
	}
}

func TestFinalizeTextDivergenceDeliversThe20260911Tail(t *testing.T) {
	// The live 2026-09-11 case: the caller streamed 186 bytes of a plan
	// statement while the authoritative final message was 787 bytes — upstream
	// had rewritten the opening, so streamed was not a prefix of final and the
	// remainder never reached the streaming caller. The missing tail must now
	// be delivered.
	streamed := "我会把缺失的日期逻辑、表单状态、响应式浮岛、图表高度和历史检查栏样式一起补齐，然后用定向语法检查、构建和多尺寸页面验收闭环。"
	final := "我会把缺失的日期逻辑、表单状态、响应式浮岛、图表高度和历史检查栏样式全部补齐，再用定向语法检查、构建和多尺寸页面验收闭环。\n\n先补 dayKeysBetween 与自定义范围激活态，再稳定历史图表高度。"
	var emitted []string
	got, err := finalizeText(streamed, final, 22, collectEmit(&emitted))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != final {
		t.Fatalf("got %q, want %q", got, final)
	}
	wantTail := final[commonPrefixLen(streamed, final):]
	if len(emitted) != 1 || emitted[0] != wantTail {
		t.Fatalf("expected the missing tail %q to be delivered, got %v", wantTail, emitted)
	}
}

func TestCommonPrefixLenStopsOnRuneBoundary(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "abc", 0},
		{"abc", "", 0},
		{"abc", "abd", 2},
		{"我会把", "我会补齐", 6},
		{"我会", "我会", 6},
	}
	for _, tc := range cases {
		if got := commonPrefixLen(tc.a, tc.b); got != tc.want {
			t.Fatalf("commonPrefixLen(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
	// A prefix that lands inside a multi-byte rune must round down.
	a := "我会"
	b := "我" + "会"[1:]
	if got := commonPrefixLen(a, b); got != 3 {
		t.Fatalf("expected the prefix to round down to a rune boundary, got %d", got)
	}
}

func TestFinalizeTextDivergenceTailIsValidUTF8(t *testing.T) {
	// End-to-end guard for the same corruption: a divergence inside a CJK rune
	// must not hand the streaming caller a tail that starts mid-character.
	streamed := "然后用定向语法检查、构建和多尺寸页面验收闭环。"
	final := "然后用定向语法检验、构建和多尺寸页面验收闭环。补齐日期逻辑。"
	var emitted []string
	if _, err := finalizeText(streamed, final, 0, collectEmit(&emitted)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(emitted) != 1 {
		t.Fatalf("expected one tail emission, got %v", emitted)
	}
	if !utf8.ValidString(emitted[0]) {
		t.Fatalf("emitted tail %q is not valid UTF-8", emitted[0])
	}
	if strings.ContainsRune(emitted[0], utf8.RuneError) {
		t.Fatalf("emitted tail %q contains a replacement character", emitted[0])
	}
}

func TestFinalizeTextPropagatesEmitError(t *testing.T) {
	wantErr := errors.New("client went away")
	_, err := finalizeText("prefix ", "prefix and tail", 0, func(string) error { return wantErr })
	if !errors.Is(err, wantErr) {
		t.Fatalf("got err %v, want %v", err, wantErr)
	}
}

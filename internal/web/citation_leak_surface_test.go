package web

import (
	"encoding/json"
	"strings"
	"testing"

	"m365-copilot2api/internal/chathub"
)

// citeMarked wraps payload in the upstream citation sentinels. The sentinels are
// private-use characters, so a client renders only their payload glued to the
// surrounding prose — that is why the leak read as "citecall_4eb000ff-...".
func citeMarked(text, payload string) string {
	return text + string(citationMarkerOpen) + "cite" + "\ue202" + payload + string(citationMarkerClose)
}

// assertNoMarkers fails when any citation sentinel or its bare payload survived.
func assertNoMarkers(t *testing.T, where, got string) {
	t.Helper()
	if strings.ContainsRune(got, citationMarkerOpen) || strings.ContainsRune(got, citationMarkerClose) {
		t.Fatalf("%s still carries a citation sentinel: %q", where, got)
	}
	if strings.Contains(got, "citeturn") || strings.Contains(got, "citecall_") {
		t.Fatalf("%s still carries a bare citation payload: %q", where, got)
	}
}

func TestCompatMetadataStripsSpokenTextMarkers(t *testing.T) {
	// spokenText is the text-to-speech rendition of the same answer, so it
	// carries the same sentinels. It rode out under "m365" untouched, which meant
	// a client reading spokenText instead of content still saw the leak.
	res := chathub.Result{SpokenText: citeMarked("已完成部署", "turn2search1")}
	got, _ := compatM365Metadata(res)["spokenText"].(string)
	assertNoMarkers(t, "m365.spokenText", got)
	if got != "已完成部署" {
		t.Fatalf("spokenText was damaged beyond the marker: %q", got)
	}
}

func TestCompatMetadataStripsSuggestedResponseMarkers(t *testing.T) {
	// Suggestions are model-authored prose rendered as clickable chips.
	res := chathub.Result{SuggestedResponses: []chathub.SuggestedResponse{{
		Text:        citeMarked("继续推送", "turn3search0"),
		CommandText: citeMarked("push", "call_abc"),
		HiddenText:  citeMarked("hidden", "turn1search2"),
	}}}
	out, ok := compatM365Metadata(res)["suggestedResponses"].([]chathub.SuggestedResponse)
	if !ok || len(out) != 1 {
		t.Fatalf("suggestedResponses missing from metadata: %#v", compatM365Metadata(res)["suggestedResponses"])
	}
	assertNoMarkers(t, "suggestion.Text", out[0].Text)
	assertNoMarkers(t, "suggestion.CommandText", out[0].CommandText)
	assertNoMarkers(t, "suggestion.HiddenText", out[0].HiddenText)
	if out[0].Text != "继续推送" {
		t.Fatalf("suggestion text was damaged: %q", out[0].Text)
	}
}

func TestSanitizedSuggestedResponsesDoesNotMutateSource(t *testing.T) {
	// The result is also handed to the usage/session bookkeeping, so rewriting it
	// in place would edit state the caller still owns.
	src := []chathub.SuggestedResponse{{Text: citeMarked("继续", "turn1search1")}}
	original := src[0].Text
	_ = sanitizedSuggestedResponses(src)
	if src[0].Text != original {
		t.Fatalf("source suggestion was mutated: %q", src[0].Text)
	}
}

func TestContentPolicyMessageStripsMarkers(t *testing.T) {
	// The refusal detail is read off res.Text BEFORE the response sanitizer runs,
	// so it was the one client-visible string that still leaked the sentinels.
	got := contentPolicyMessage(citeMarked("这个请求被拒绝了。", "turn1search5"))
	assertNoMarkers(t, "content policy message", got)
	if got != "这个请求被拒绝了。" {
		t.Fatalf("refusal wording was damaged: %q", got)
	}
}

func TestContentPolicyMessageFallsBackWhenOnlyMarkersRemain(t *testing.T) {
	// A detail that is nothing but a sentinel becomes empty after stripping; it
	// must fall through to the generic wording instead of an empty message.
	got := contentPolicyMessage(citeMarked("", "turn1search5"))
	if !strings.Contains(got, "content policy") {
		t.Fatalf("empty-after-strip detail did not fall back: %q", got)
	}
}

func TestContentPolicyMessageKeepsCleanDetail(t *testing.T) {
	const detail = "I can't help with that request."
	if got := contentPolicyMessage(detail); got != detail {
		t.Fatalf("clean detail was rewritten: %q", got)
	}
}

func TestCompatMetadataLeavesCleanResultUntouched(t *testing.T) {
	res := chathub.Result{
		SpokenText:         "plain spoken text",
		SuggestedResponses: []chathub.SuggestedResponse{{Text: "plain suggestion"}},
	}
	m := compatM365Metadata(res)
	if got, _ := m["spokenText"].(string); got != "plain spoken text" {
		t.Fatalf("clean spokenText was rewritten: %q", got)
	}
	// The metadata has to stay JSON-encodable after the copy.
	if _, err := json.Marshal(m); err != nil {
		t.Fatalf("metadata is no longer serializable: %v", err)
	}
}

func TestSanitizedSemanticEventStripsProjectedFields(t *testing.T) {
	// /api/chat/stream emits a projected "semantic" view of each upstream event.
	// Text/HiddenText/Queries are what an API consumer renders, so they leaked the
	// sentinels the same way assistant content did — confirmed live: 82 semantic
	// frames of one search-heavy turn carried U+E200/U+E201/U+E202.
	marker := string(citationMarkerOpen) + "citeturn2search1" + string(citationMarkerClose)
	in := chathub.SemanticEvent{
		Kind:        "search.progress",
		Text:        "range-over-func changed. " + marker,
		HiddenText:  "hidden " + marker,
		Queries:     []string{"go 1.24 " + marker, "clean query"},
		ContentType: "SearchResults",
	}

	got := sanitizedSemanticEvent(in)
	if strings.ContainsRune(got.Text, citationMarkerOpen) || strings.Contains(got.Text, "citeturn") {
		t.Fatalf("semantic text still carries a marker: %q", got.Text)
	}
	if got.Text != "range-over-func changed. " {
		t.Fatalf("stripping damaged the surrounding prose: %q", got.Text)
	}
	if strings.ContainsRune(got.HiddenText, citationMarkerOpen) {
		t.Fatalf("semantic hiddenText still carries a marker: %q", got.HiddenText)
	}
	for i, q := range got.Queries {
		if strings.ContainsRune(q, citationMarkerOpen) {
			t.Fatalf("query %d still carries a marker: %q", i, q)
		}
	}
	if got.Queries[1] != "clean query" {
		t.Fatalf("a clean query was rewritten: %q", got.Queries[1])
	}
	if got.Kind != "search.progress" || got.ContentType != "SearchResults" {
		t.Fatalf("non-text fields were altered: %+v", got)
	}
}

func TestSanitizedSemanticEventDoesNotMutateSource(t *testing.T) {
	// The Queries slice is shared with the caller's event, so stripping in place
	// would rewrite the upstream data the Raw passthrough is supposed to preserve.
	marker := string(citationMarkerOpen) + "citeturn2search1" + string(citationMarkerClose)
	original := []string{"go 1.24 " + marker}
	in := chathub.SemanticEvent{Text: "t " + marker, Queries: original}

	_ = sanitizedSemanticEvent(in)
	if !strings.ContainsRune(original[0], citationMarkerOpen) {
		t.Fatalf("source query slice was mutated: %q", original[0])
	}
	if !strings.ContainsRune(in.Text, citationMarkerOpen) {
		t.Fatalf("source event text was mutated: %q", in.Text)
	}
}

func TestSanitizedSemanticEventKeepsRawVerbatim(t *testing.T) {
	// Raw is the debug passthrough of exactly what the upstream sent; cleaning it
	// would defeat its only purpose.
	marker := string(citationMarkerOpen) + "citeturn2search1" + string(citationMarkerClose)
	raw := chathub.Event{Arguments: json.RawMessage(`{"text":"a ` + marker + `"}`)}
	got := sanitizedSemanticEvent(chathub.SemanticEvent{Text: "x " + marker, Raw: raw})
	if !strings.ContainsRune(string(got.Raw.Arguments), citationMarkerOpen) {
		t.Fatalf("Raw passthrough was sanitized: %q", got.Raw.Arguments)
	}
}

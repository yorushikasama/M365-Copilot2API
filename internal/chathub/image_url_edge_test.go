package chathub

import (
	"encoding/json"
	"testing"
)

// A "data:image/..." value with no comma has no base64 payload. isImageURL used
// to index SplitN(s, ",", 2)[1] unconditionally, so such a value panicked while
// walking upstream frames.
func TestIsImageURLHandlesDataURIWithoutPayload(t *testing.T) {
	for _, s := range []string{"data:image/png", "data:image/", "data:image/png;base64"} {
		if isImageURL(s) {
			t.Errorf("%q has no payload and must not count as an image", s)
		}
	}
}

func TestImageURLsSurvivesMalformedDataURI(t *testing.T) {
	raw := []json.RawMessage{
		json.RawMessage(`{"data":"data:image/png"}`),
		json.RawMessage(`{"src":"https://cdn.example.com/image/ok.png"}`),
	}
	got := imageURLs(raw)
	if len(got) != 1 || got[0] != "https://cdn.example.com/image/ok.png" {
		t.Fatalf("expected only the valid URL, got %v", got)
	}
}

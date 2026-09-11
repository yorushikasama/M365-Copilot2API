package web

import (
	"testing"

	"m365-copilot2api/internal/chathub"
)

// A URL the model merely mentioned in prose is not a generated image. Accepting
// one used to end the rotation loop with a non-empty list, and
// downloadDesignerImage then refused the host as a non-retryable 502 — so a
// citation or a reference thumbnail in the answer text turned a recoverable
// "no image this time" into a hard failure without ever trying another account.
func TestImageURLsFromResultKeepsOnlyDesignerHosts(t *testing.T) {
	cases := []struct {
		name string
		text string
		want int
	}{
		{
			name: "documentation link is not a generated image",
			text: "I could not generate that. See https://example.com/docs/image-guide.png for the policy.",
			want: 0,
		},
		{
			name: "learn.microsoft.com media link is not a generated image",
			text: "Style reference: https://learn.microsoft.com/media/designer-image.png",
			want: 0,
		},
		{
			name: "a real Designer URL is still accepted",
			text: "Here it is: https://designerapp.officeapps.live.com/api/v1/get?path=out/dalle-1.png&dcHint=eu",
			want: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := imageURLsFromResult(chathub.Result{Text: tc.text})
			if len(got) != tc.want {
				t.Fatalf("imageURLsFromResult returned %d URLs (%v), want %d", len(got), got, tc.want)
			}
			for _, u := range got {
				if !isDesignerImageURL(u) {
					t.Fatalf("kept %q, which downloadDesignerImage will refuse as an unsupported host", u)
				}
			}
		})
	}
}

// The structured field is authoritative: chathub already resolved those, so they
// must survive untouched even though the prose scan is now filtered.
func TestImageURLsFromResultPrefersStructuredImages(t *testing.T) {
	res := chathub.Result{
		Images: []string{"https://designerapp.officeapps.live.com/api/v1/get?path=a.png"},
		Text:   "also see https://example.com/other.png",
	}
	got := imageURLsFromResult(res)
	if len(got) != 1 || got[0] != res.Images[0] {
		t.Fatalf("structured images must be returned as-is, got %v", got)
	}
}

// Every URL kept by the fallback has to be one downloadDesignerImage accepts,
// otherwise the rotation loop breaks out on an image it can never fetch.
func TestImageProseFallbackAgreesWithDownloadHostCheck(t *testing.T) {
	text := "Options: https://designerapp.officeapps.live.com/api/v1/get?path=ok.png " +
		"and https://cdn.contoso.com/thumb.jpg and https://example.com/image/x.webp"
	for _, u := range imageURLsFromResult(chathub.Result{Text: text}) {
		if !isDesignerImageURL(u) {
			t.Fatalf("fallback kept %q but the download path rejects that host", u)
		}
	}
}

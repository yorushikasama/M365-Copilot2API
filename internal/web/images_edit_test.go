package web

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"m365-copilot2api/internal/chathub"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestImageEditsValidation(t *testing.T) {
	t.Run("method", func(t *testing.T) {
		w := httptest.NewRecorder()
		(&Server{}).imageEdits(w, httptest.NewRequest(http.MethodGet, "/v1/images/edits", nil))
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status=%d want %d", w.Code, http.StatusMethodNotAllowed)
		}
	})

	t.Run("prompt", func(t *testing.T) {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		part, err := writer.CreateFormFile("image", "image.png")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = part.Write([]byte("not reached without a prompt"))
		_ = writer.Close()
		r := httptest.NewRequest(http.MethodPost, "/v1/images/edits", &body)
		r.Header.Set("Content-Type", writer.FormDataContentType())
		w := httptest.NewRecorder()
		(&Server{}).imageEdits(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d want %d", w.Code, http.StatusBadRequest)
		}
	})

	t.Run("image", func(t *testing.T) {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		_ = writer.WriteField("prompt", "make it blue")
		_ = writer.Close()
		r := httptest.NewRequest(http.MethodPost, "/v1/images/edits", &body)
		r.Header.Set("Content-Type", writer.FormDataContentType())
		w := httptest.NewRecorder()
		(&Server{}).imageEdits(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d want %d", w.Code, http.StatusBadRequest)
		}
	})
}

// TestImageEditReferenceSize pins the reference-image ceiling. Both entry points
// must refuse the same oversized image, and the ceiling itself must survive the
// base64 round trip: a 5 MiB image is legal even though it arrives as ~6.7 MiB
// of encoded text.
func TestImageEditReferenceSize(t *testing.T) {
	oversized := make([]byte, maxImageEditImageBytes+1)

	t.Run("multipart refuses one byte over", func(t *testing.T) {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		_ = writer.WriteField("prompt", "make it blue")
		part, err := writer.CreateFormFile("image", "image.png")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(oversized); err != nil {
			t.Fatal(err)
		}
		_ = writer.Close()

		r := httptest.NewRequest(http.MethodPost, "/v1/images/edits", &body)
		r.Header.Set("Content-Type", writer.FormDataContentType())
		w := httptest.NewRecorder()
		(&Server{}).imageEdits(w, r)
		if w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status=%d want %d (body=%s)", w.Code, http.StatusRequestEntityTooLarge, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), imageEditLimitError()) {
			t.Fatalf("body=%s want it to mention %q", w.Body.String(), imageEditLimitError())
		}
	})

	t.Run("json attachment refuses one byte over", func(t *testing.T) {
		// A JSON caller bypasses the multipart handler, so this path has to
		// apply the ceiling itself.
		encoded := base64.StdEncoding.EncodeToString(oversized)
		payload := fmt.Sprintf(
			`{"prompt":"make it blue","operation":"edit","attachments":[{"type":"image","url":"data:image/png;base64,%s"}]}`,
			encoded,
		)
		r := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(payload))
		w := httptest.NewRecorder()
		(&Server{}).imageGenerations(w, r)
		if w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status=%d want %d (body=%s)", w.Code, http.StatusRequestEntityTooLarge, w.Body.String())
		}
	})

	t.Run("json edit without a reference image is a bad request", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/v1/images/generations",
			strings.NewReader(`{"prompt":"make it blue","operation":"edit"}`))
		w := httptest.NewRecorder()
		(&Server{}).imageGenerations(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d want %d (body=%s)", w.Code, http.StatusBadRequest, w.Body.String())
		}
	})
}

func TestEditAttachmentBytes(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString(make([]byte, maxImageEditImageBytes))

	// The whole point of the ceiling being 5 MiB is that a 5 MiB image is not
	// "over" it, even though base64 makes it look bigger on the wire.
	if got := editAttachmentBytes(chathub.Attachment{URL: "data:image/png;base64," + encoded}); got != maxImageEditImageBytes {
		t.Fatalf("decoded size=%d want %d", got, maxImageEditImageBytes)
	}
	// A remote reference cannot be measured without fetching it, so it must not
	// be reported as oversized here — chathub's download cap owns that case.
	if got := editAttachmentBytes(chathub.Attachment{URL: "https://example.com/a.png"}); got != 0 {
		t.Fatalf("remote size=%d want 0", got)
	}
	if got := editAttachmentBytes(chathub.Attachment{URL: "data:image/svg+xml,%3Csvg/%3E"}); got != len("%3Csvg/%3E") {
		t.Fatalf("plain data url size=%d want %d", got, len("%3Csvg/%3E"))
	}
}

// The upload ceiling must not be inherited by the *download* path: generated
// PNGs are routinely larger than a 5 MiB reference image.
func TestGeneratedImageCeilingIsNotTheUploadCeiling(t *testing.T) {
	if maxGeneratedImageBytes <= maxImageEditImageBytes {
		t.Fatalf("maxGeneratedImageBytes=%d must stay above maxImageEditImageBytes=%d",
			maxGeneratedImageBytes, maxImageEditImageBytes)
	}
	if maxImageEditRequestBytes <= maxImageEditImageBytes {
		t.Fatalf("maxImageEditRequestBytes=%d must leave room for base64 inflation of a %d byte image",
			maxImageEditRequestBytes, maxImageEditImageBytes)
	}
	// base64 expands 3 bytes to 4, so the request ceiling has to be at least
	// 4/3 of the image ceiling or a legal upload would 413 on the way in.
	if min := maxImageEditImageBytes / 3 * 4; maxImageEditRequestBytes < min {
		t.Fatalf("maxImageEditRequestBytes=%d cannot carry a base64-encoded %d byte image (needs >= %d)",
			maxImageEditRequestBytes, maxImageEditImageBytes, min)
	}
}

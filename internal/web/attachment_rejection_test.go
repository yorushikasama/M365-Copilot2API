package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"m365-copilot2api/internal/chathub"
)

// TestAttachmentRejectionIsAClientError proves a refused image is reported as a
// 400 the caller can act on, not as a gateway error.
func TestAttachmentRejectionIsAClientError(t *testing.T) {
	err := fmt.Errorf("upload attachment: attachment 0: %w (upstream said ImageSanitizationError)", chathub.ErrAttachmentRejected)
	if got := ClassifyError(err); got != CategoryAttachmentRejected {
		t.Fatalf("category = %s, want %s", got, CategoryAttachmentRejected)
	}
	if got := upstreamStatus(err); got != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", got, http.StatusBadRequest)
	}
	rec := httptest.NewRecorder()
	writeUpstreamError(rec, err)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("response status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "invalid_image") {
		t.Fatalf("response body lacks the invalid_image code: %s", body)
	}
	if strings.Contains(body, "upstream request failed") {
		t.Fatalf("response body still hides the cause behind a generic message: %s", body)
	}
}

// TestAttachmentRejectionDoesNotSidelineTheAccount guards the pool: the upstream
// refused the caller's bytes, so cooling the account down would take a healthy
// account out of rotation for someone else's bad image.
func TestAttachmentRejectionDoesNotSidelineTheAccount(t *testing.T) {
	ResetGlobalCircuit()
	t.Cleanup(ResetGlobalCircuit)
	h := newAccountHealth()
	const id = "acct-1"
	h.MarkFailure(id, fmt.Errorf("upload attachment: %w", chathub.ErrAttachmentRejected), 15*time.Minute)
	if !h.Available(id) {
		t.Fatal("a refused attachment must not put the account in cooldown")
	}
	if h.AuthFailReason(id) != "" {
		t.Fatal("a refused attachment must not mark the account as auth-failed")
	}
	if !h.ImageGenAvailable(id) {
		t.Fatal("a refused attachment must not cool down image generation")
	}
	if GlobalCircuitIsOpen() {
		t.Fatal("a refused attachment must not trip the global circuit")
	}
}

// TestAttachmentUploadOutageStaysRetryable keeps a transient upload failure on
// the retryable path so it is not misreported as a bad request, and pins the
// status at 503: it used to fall through to a blanket 502, which told the client
// the gateway was broken rather than that the upload should simply be retried.
func TestAttachmentUploadOutageStaysRetryable(t *testing.T) {
	err := fmt.Errorf("upload attachment: %w: upstream http 500", chathub.ErrAttachmentUploadFailed)
	if errors.Is(err, chathub.ErrAttachmentRejected) {
		t.Fatal("a transient upload failure must not read as a refusal")
	}
	if got := upstreamStatus(err); got != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", got, http.StatusServiceUnavailable)
	}
	if !imageFailoverWorthwhile(err) {
		t.Fatal("a transient upload failure must rotate onto another account")
	}
}

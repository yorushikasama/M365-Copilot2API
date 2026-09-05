package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// A download failure has to say whether retrying helps. The whole path used to
// collapse every cause into one permanent 502, which is why a transient blip
// looked identical to an image that can never be fetched.
func TestClassifyDesignerStatus(t *testing.T) {
	cases := []struct {
		status        int
		wantRetryable bool
		wantExpired   bool
	}{
		{http.StatusUnauthorized, false, true},
		{http.StatusForbidden, false, true},
		{http.StatusRequestTimeout, true, false},
		{http.StatusTooManyRequests, true, false},
		{http.StatusInternalServerError, true, false},
		{http.StatusBadGateway, true, false},
		{http.StatusServiceUnavailable, true, false},
		{http.StatusNotFound, false, false},
		{http.StatusBadRequest, false, false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("HTTP %d", tc.status), func(t *testing.T) {
			de := classifyDesignerStatus(tc.status)
			if de.Retryable != tc.wantRetryable {
				t.Fatalf("Retryable = %v, want %v", de.Retryable, tc.wantRetryable)
			}
			if de.Expired != tc.wantExpired {
				t.Fatalf("Expired = %v, want %v", de.Expired, tc.wantExpired)
			}
			if de.Status != tc.status {
				t.Fatalf("Status = %d, want %d", de.Status, tc.status)
			}
			if de.Public == "" {
				t.Fatal("every failure needs a client-visible reason")
			}
			// The status has to survive wrapping so the loop can act on it.
			var got *designerDownloadError
			if !errors.As(fmt.Errorf("download: %w", de), &got) || got.Status != tc.status {
				t.Fatal("wrapping lost the download error")
			}
		})
	}
}

func TestDesignerDownloadFailureResponse(t *testing.T) {
	t.Run("transient failures invite a retry", func(t *testing.T) {
		for _, err := range []error{classifyDesignerStatus(http.StatusServiceUnavailable), classifyDesignerStatus(http.StatusUnauthorized)} {
			status, code, msg := designerDownloadFailure(err)
			if status != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503 for %v", status, err)
			}
			if code != "upstream_unavailable" {
				t.Fatalf("code = %q, want upstream_unavailable", code)
			}
			if !strings.Contains(msg, "retry") {
				t.Fatalf("message %q must tell the caller retrying helps", msg)
			}
		}
	})
	t.Run("permanent failures stay 502 and explain themselves", func(t *testing.T) {
		de := classifyDesignerStatus(http.StatusNotFound)
		status, code, msg := designerDownloadFailure(de)
		if status != http.StatusBadGateway || code != "upstream_error" {
			t.Fatalf("status/code = %d/%q, want 502/upstream_error", status, code)
		}
		if msg != de.Public {
			t.Fatalf("message = %q, want the specific reason %q instead of a blanket string", msg, de.Public)
		}
		// The internal cause carries the raw upstream status and must not leak.
		if strings.Contains(msg, "HTTP 404") {
			t.Fatalf("message %q leaked the internal cause", msg)
		}
	})
	t.Run("unclassified errors keep the old blanket 502", func(t *testing.T) {
		status, code, _ := designerDownloadFailure(errors.New("boom"))
		if status != http.StatusBadGateway || code != "upstream_error" {
			t.Fatalf("status/code = %d/%q, want 502/upstream_error", status, code)
		}
	})
}

func TestIsRetryableDesignerError(t *testing.T) {
	if !isRetryableDesignerError(fmt.Errorf("wrapped: %w", classifyDesignerStatus(http.StatusBadGateway))) {
		t.Fatal("a wrapped 502 from Designer is retryable")
	}
	if isRetryableDesignerError(classifyDesignerStatus(http.StatusUnauthorized)) {
		t.Fatal("a rejected token needs a fresh token, not a blind retry")
	}
	if isRetryableDesignerError(errors.New("boom")) {
		t.Fatal("an unclassified error must not be retried")
	}
}

// The host guard runs before any request is made, so it must be terminal: no
// number of retries turns a foreign host into a Designer image.
func TestDownloadDesignerImageRejectsForeignHostWithoutRetry(t *testing.T) {
	_, _, err := downloadDesignerImage(context.Background(), "https://example.com/a.png", "token")
	if err == nil {
		t.Fatal("expected a foreign host to be rejected")
	}
	if isRetryableDesignerError(err) {
		t.Fatal("an unsupported host must not be retried")
	}
	status, _, _ := designerDownloadFailure(err)
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", status)
	}
}

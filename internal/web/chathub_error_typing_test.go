package web

import (
	"m365-copilot2api/internal/chathub"
	"testing"
)

// The three chathub paths that used to return a bare fmt.Errorf — an unmapped
// SignalR type-3 error code, and the response-deadline exit — are invisible to
// ClassifyError, which only inspects *DialError. They fell through to
// CategoryUnknown, so IsRetryable was false and the turn became a hard 502 with
// zero failover. These assert the kinds they now carry stay retryable.
func TestUnmappedUpstreamErrorKindsAreRetryable(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCat  ErrorCategory
		wantSafe bool
	}{
		{
			name:     "unmapped signalr error code before any token",
			err:      &chathub.DialError{Kind: "UPSTREAM_STRUCTURED"},
			wantCat:  CategoryUpstreamStructured,
			wantSafe: true,
		},
		{
			name:     "unmapped signalr error code mid-answer must not be re-issued",
			err:      &chathub.DialError{Kind: "UPSTREAM_STRUCTURED", Streamed: true},
			wantCat:  CategoryUpstreamStructured,
			wantSafe: false,
		},
		{
			name:     "response deadline with no completion frame",
			err:      &chathub.DialError{Kind: "WS_READ_TIMEOUT"},
			wantCat:  CategoryWSReadTimeout,
			wantSafe: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyError(tc.err); got != tc.wantCat {
				t.Fatalf("ClassifyError = %q, want %q", got, tc.wantCat)
			}
			if !IsRetryable(tc.err) {
				t.Fatal("IsRetryable = false; a bare error here gets zero failover attempts")
			}
			if got := chathub.IsSafeToRetry(tc.err); got != tc.wantSafe {
				t.Fatalf("IsSafeToRetry = %v, want %v", got, tc.wantSafe)
			}
		})
	}
}

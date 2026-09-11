package web

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"m365-copilot2api/internal/chathub"
)

// A bare "upstream result error: InternalError" used to bypass every typed
// check: not a DialError, not a MeteringError, not any sentinel. It classified
// as UNKNOWN, so IsRetryable said no and — because IsSafeToRetry only accepts
// DialError — the failover gate also said no. 29 InternalError failures on
// 2026-09-11 all went back to callers as 502 with zero failover. The chathub
// client now returns a DialError with Kind "UPSTREAM_STRUCTURED"; pin the whole
// chain so it cannot silently regress back to a dead-end error.
func TestUpstreamInternalErrorIsFailoverWorthwhile(t *testing.T) {
	err := &chathub.DialError{Status: 0, Kind: "UPSTREAM_STRUCTURED", Streamed: false}

	if got := ClassifyError(err); got != CategoryUpstreamStructured {
		t.Fatalf("ClassifyError = %q, want %q", got, CategoryUpstreamStructured)
	}
	if !IsRetryable(err) {
		t.Fatal("IsRetryable must be true: InternalError is a transient upstream fault, not a dead end")
	}
	if !chathub.IsSafeToRetry(err) {
		t.Fatal("IsSafeToRetry must be true when nothing was streamed; otherwise failover silently refuses")
	}
	if got := upstreamStatus(err); got != http.StatusBadGateway {
		t.Fatalf("upstreamStatus = %d, want 502 when failover is exhausted", got)
	}
}

func TestUpstreamInternalErrorMidStreamIsNotFailoverWorthwhile(t *testing.T) {
	err := &chathub.DialError{Status: 0, Kind: "UPSTREAM_STRUCTURED", Streamed: true}
	if chathub.IsSafeToRetry(err) {
		t.Fatal("an InternalError after content streamed must not be retried: re-issuing would duplicate output")
	}
}

// The classification must survive the fmt.Errorf wrapper the read loop used to
// apply, so a future change cannot quietly turn the error back into a string.
func TestUpstreamInternalErrorWrappedStillClassifies(t *testing.T) {
	err := fmt.Errorf("chat: %w", &chathub.DialError{Status: 0, Kind: "UPSTREAM_STRUCTURED"})
	if got := ClassifyError(err); got != CategoryUpstreamStructured {
		t.Fatalf("ClassifyError = %q, want %q", got, CategoryUpstreamStructured)
	}
	if errors.Is(err, chathub.ErrRateLimitNotice) {
		t.Fatal("InternalError must not classify as a rate-limit notice")
	}
}

package web

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"m365-copilot2api/internal/chathub"
)

func TestTransportRetryBudget(t *testing.T) {
	t.Run("plenty of time left", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		budget, enough := transportRetryBudget(ctx)
		if !enough {
			t.Fatalf("budget %v must be enough for a second attempt", budget)
		}
		if budget <= minTransportRetryBudget {
			t.Fatalf("budget = %v, want well above %v", budget, minTransportRetryBudget)
		}
	})
	t.Run("too little time left", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), minTransportRetryBudget/2)
		defer cancel()
		if _, enough := transportRetryBudget(ctx); enough {
			t.Fatal("a second attempt cut short by the caller's deadline is not worth making")
		}
	})
	t.Run("already expired", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
		defer cancel()
		<-ctx.Done()
		if _, enough := transportRetryBudget(ctx); enough {
			t.Fatal("an exhausted budget must not authorize a retry")
		}
	})
	t.Run("no deadline is unbounded", func(t *testing.T) {
		budget, enough := transportRetryBudget(context.Background())
		if !enough {
			t.Fatal("an unbounded context is always worth retrying")
		}
		if budget != 0 {
			t.Fatalf("budget = %v, want 0 so the clamp is skipped", budget)
		}
	})
}

// The gate has to separate "upstream went quiet before saying anything", which
// can be re-issued on another account, from everything that would either
// duplicate output or fail again for the same reason.
func TestShouldFailoverTransport(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"silent read timeout", &chathub.DialError{Status: 0, Kind: "WS_READ_TIMEOUT"}, true},
		{"read timeout after content streamed", &chathub.DialError{Status: 0, Kind: "WS_READ_TIMEOUT", Streamed: true}, false},
		{"upstream closed the socket early", &chathub.DialError{Status: 0, Kind: "TCP"}, true},
		{"handshake failure", &chathub.DialError{Status: 0, Kind: "WS_HANDSHAKE"}, true},
		{"wrapped read timeout", fmt.Errorf("chat: %w", &chathub.DialError{Status: 0, Kind: "WS_READ_TIMEOUT"}), true},
		{"client hung up", &chathub.DialError{Status: 0, Kind: "CLIENT_CANCELED"}, false},
		{"bare context cancel", context.Canceled, false},
		{"auth expiry is handled by its own branch", &chathub.DialError{Status: 401}, false},
		{"unclassified error carries no phase", errors.New("boom"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if _, got := shouldFailoverTransport(ctx, tc.err); got != tc.want {
				t.Fatalf("shouldFailoverTransport(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// A retryable error must not authorize a retry once the request has no budget
// left; otherwise a slow upstream would silently cost the caller two full
// timeouts before failing.
func TestShouldFailoverTransportRespectsExhaustedBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	err := &chathub.DialError{Status: 0, Kind: "WS_READ_TIMEOUT"}
	if !IsRetryable(err) || !chathub.IsSafeToRetry(err) {
		t.Fatal("precondition: this error is otherwise eligible for failover")
	}
	if _, got := shouldFailoverTransport(ctx, err); got {
		t.Fatal("an eligible error must still be refused when no budget remains")
	}
}

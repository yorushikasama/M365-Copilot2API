package web

import (
	"context"
	"errors"
	"testing"
	"time"

	"m365-copilot2api/internal/chathub"
)

func TestAccountConcurrencyLimitsAndReleasesSlots(t *testing.T) {
	t.Setenv("M365_ACCOUNT_DEFAULT_CONCURRENCY", "2")
	limiter := newAccountConcurrency()
	release1, err := limiter.Acquire(context.Background(), "account-a")
	if err != nil {
		t.Fatal(err)
	}
	release2, err := limiter.Acquire(context.Background(), "account-a")
	if err != nil {
		t.Fatal(err)
	}
	if limiter.Available("account-a") {
		t.Fatal("account remained available at its configured limit")
	}
	if !limiter.Available("account-b") {
		t.Fatal("one full account must not block another account")
	}
	release1()
	if !limiter.Available("account-a") {
		t.Fatal("released slot was not returned")
	}
	release1()
	release2()
}

func TestAccountConcurrencyWaitHonorsCancellation(t *testing.T) {
	t.Setenv("M365_ACCOUNT_DEFAULT_CONCURRENCY", "1")
	limiter := newAccountConcurrency()
	release, err := limiter.Acquire(context.Background(), "account-a")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := limiter.Acquire(ctx, "account-a"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire() error = %v, want deadline exceeded", err)
	}
}

func TestAccountConcurrencyUsesDocumentedDefault(t *testing.T) {
	t.Setenv("M365_ACCOUNT_DEFAULT_CONCURRENCY", "")
	limiter := newAccountConcurrency()
	if limiter.limit != defaultAccountConcurrency {
		t.Fatalf("limit = %d, want %d", limiter.limit, defaultAccountConcurrency)
	}
}

// The console-editable account concurrency must actually gate traffic; before
// bindLimitProvider existed the persisted value was validated but never applied.
func TestAccountConcurrencyFollowsRuntimeSettings(t *testing.T) {
	t.Setenv("M365_ACCOUNT_DEFAULT_CONCURRENCY", "")
	t.Setenv("M365_ACCOUNT_CONCURRENCY_LIMIT", "")
	limiter := newAccountConcurrency()
	configured := 1
	limiter.bindLimitProvider(func() int { return configured })

	release, err := limiter.Acquire(context.Background(), "account-a")
	if err != nil {
		t.Fatal(err)
	}
	if limiter.Available("account-a") {
		t.Fatal("limit of 1 was not enforced from runtime settings")
	}
	if got := limiter.Snapshot()["limit"]; got != 1 {
		t.Fatalf("Snapshot limit = %v, want 1", got)
	}

	configured = 3
	if !limiter.Available("account-a") {
		t.Fatal("raising the configured limit did not take effect without a restart")
	}
	release()
}

// An explicit env override must keep winning over persisted console settings.
func TestAccountConcurrencyEnvOverridesRuntimeSettings(t *testing.T) {
	t.Setenv("M365_ACCOUNT_DEFAULT_CONCURRENCY", "2")
	limiter := newAccountConcurrency()
	limiter.bindLimitProvider(func() int { return 50 })
	if got := limiter.currentLimit(); got != 2 {
		t.Fatalf("currentLimit() = %d, want 2 (explicit env must win)", got)
	}
}

// The legacy alias must remain usable so existing deployments do not silently
// lose their configured ceiling.
func TestAccountConcurrencyAcceptsLegacyEnvAlias(t *testing.T) {
	t.Setenv("M365_ACCOUNT_DEFAULT_CONCURRENCY", "")
	t.Setenv("M365_ACCOUNT_CONCURRENCY_LIMIT", "5")
	limiter := newAccountConcurrency()
	if got := limiter.currentLimit(); got != 5 {
		t.Fatalf("currentLimit() = %d, want 5 from legacy alias", got)
	}
}

// Connection-level faults are retried on a fresh connection for the same
// account; account-level faults (quota, auth) are not, and neither is anything
// that already streamed content to the client.
func TestRetryableTransportErrorClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"ws protocol corruption", &chathub.DialError{Kind: "WS_PROTOCOL"}, true},
		{"handshake failure", &chathub.DialError{Kind: "WS_HANDSHAKE"}, true},
		{"tcp reset", &chathub.DialError{Kind: "TCP"}, true},
		{"tls failure", &chathub.DialError{Kind: "TLS"}, true},
		{"quota is an account fault", &chathub.DialError{Kind: "QUOTA_429", Status: 429}, false},
		{"auth is an account fault", &chathub.DialError{Kind: "AUTH_EXPIRED_401", Status: 401}, false},
		{"read timeout after first token", &chathub.DialError{Kind: "WS_READ_TIMEOUT", Streamed: true}, false},
		{"read timeout before first token", &chathub.DialError{Kind: "WS_READ_TIMEOUT"}, false},
		{"protocol error after streaming", &chathub.DialError{Kind: "WS_PROTOCOL", Streamed: true}, false},
		{"plain error", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := retryableTransportError(tt.err); got != tt.want {
				t.Fatalf("retryableTransportError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// A transport retry must not mutate the caller's request, and must force a
// freshly dialed connection instead of reusing a parked one.
func TestFreshConnectionRequestCopiesRequest(t *testing.T) {
	original := chathub.Request{Text: "hello", ConversationID: "conv-1"}
	retry := freshConnectionRequest(original)
	if !retry.BypassPool {
		t.Fatal("retry request must bypass the pool")
	}
	if original.BypassPool {
		t.Fatal("freshConnectionRequest must not mutate the caller's request")
	}
	if retry.Text != "hello" || retry.ConversationID != "conv-1" {
		t.Fatalf("retry request lost fields: %+v", retry)
	}
}

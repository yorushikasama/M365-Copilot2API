package web

import (
	"testing"

	"m365-copilot2api/internal/chathub"
)

func TestImageThrottleDoesNotSidelineChat(t *testing.T) {
	h := newAccountHealth()
	s := &Server{accountPool: h, settings: &settingsStore{v: defaultRuntimeSettings()}}
	err := &chathub.MeteringError{
		Cause:    chathub.ErrMeteringThrottled,
		Metering: []any{map[string]any{"meterError": "ImageGenSystemCapacityThrottled", "hasAccess": false}},
	}
	s.recordAccountResultForCapability("account-a", chathub.Result{}, err, "ImageGeneration")
	if h.ImageGenAvailable("account-a") {
		t.Fatal("image throttle did not cool down image generation")
	}
	if !h.Available("account-a") {
		t.Fatal("image throttle sidelined the account for chat")
	}

	// The same upstream error on a text request is a general throttle and must
	// still cool the account down.
	s.recordAccountResultForCapability("account-b", chathub.Result{}, err, "")
	if h.Available("account-b") {
		t.Fatal("chat throttle left the account available")
	}
}

func TestMarkImageThrottleSeparatesDailyQuotaFromCapacity(t *testing.T) {
	h := newAccountHealth()
	s := &Server{accountPool: h}

	s.markImageThrottle("daily", chathub.ErrImageLimit)
	if h.ImageGenAvailable("daily") || !h.Available("daily") {
		t.Fatal("daily image quota must cool image generation only")
	}
	until, ok := h.ImageGenCooldownUntil("daily")
	if !ok || until.IsZero() {
		t.Fatalf("daily cooldown missing: until=%v ok=%v", until, ok)
	}

	s.markImageThrottle("capacity", chathub.ErrMeteringThrottled)
	if h.ImageGenAvailable("capacity") || !h.Available("capacity") {
		t.Fatal("capacity throttle must cool image generation only")
	}

	// An unrelated failure is not an image throttle and must not touch the
	// image-generation cooldown.
	s.markImageThrottle("other", chathub.ErrOffensiveContent)
	if !h.ImageGenAvailable("other") {
		t.Fatal("non-throttle error cooled down image generation")
	}
}

func TestImageFailoverWorthwhileOnlyForThrottles(t *testing.T) {
	for _, err := range []error{chathub.ErrImageLimit, chathub.ErrMeteringThrottled, chathub.ErrRateLimitNotice, errImageQuotaRefused} {
		if !imageFailoverWorthwhile(err) {
			t.Fatalf("expected failover for %v", err)
		}
	}
	for _, err := range []error{nil, chathub.ErrOffensiveContent, chathub.ErrEmptyCompletion} {
		if imageFailoverWorthwhile(err) {
			t.Fatalf("unexpected failover for %v", err)
		}
	}
}

func TestNextImageAccountSkipsTriedAndThrottled(t *testing.T) {
	store := testAccountFiles(t)
	h := newAccountHealth()
	s := &Server{tokens: store, accountPool: h, accountConcurrency: newAccountConcurrency(), settings: &settingsStore{v: defaultRuntimeSettings()}}
	accounts := store.List()
	if len(accounts) != 3 {
		t.Fatalf("expected 3 accounts, got %d", len(accounts))
	}
	tried := map[string]bool{accounts[0].ID: true}
	s.markImageThrottle(accounts[1].ID, chathub.ErrImageLimit)

	next, err := s.nextImageAccount(tried)
	if err != nil {
		t.Fatal(err)
	}
	if next.ID != accounts[2].ID {
		t.Fatalf("selected %s, want the only untried and unthrottled account %s", next.ID, accounts[2].ID)
	}

	// With every account either tried or image-throttled there is nothing left
	// to fail over to, and the caller must report the throttle instead.
	tried[accounts[2].ID] = true
	if _, err := s.nextImageAccount(tried); err == nil {
		t.Fatal("failover returned an account when none were eligible")
	}
}

package web

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

// Image generation is metered separately from chat, so an image cooldown must
// not sideline the account for chat traffic.
func TestImageGenCooldownDoesNotBlockChat(t *testing.T) {
	h := newAccountHealth()
	const id = "u-1"

	h.MarkImageGenTokensThrottled(id)
	if h.ImageGenAvailable(id) {
		t.Fatal("image generation must be unavailable after a daily quota throttle")
	}
	if !h.Available(id) {
		t.Fatal("image quota must not cool the account down for chat")
	}

	h2 := newAccountHealth()
	h2.MarkImageGenSystemThrottled(id)
	if h2.ImageGenAvailable(id) {
		t.Fatal("image generation must be unavailable after a system capacity throttle")
	}
	if !h2.Available(id) {
		t.Fatal("system capacity throttle must not cool the account down for chat")
	}
}

// An image-only cooldown leaves no entry in the general cooldown map, so it has
// to be pruned by its own deadline rather than by cleanupExpiredCooldownLocked.
func TestImageGenCooldownExpires(t *testing.T) {
	h := newAccountHealth()
	const id = "u-1"

	h.MarkImageGenTokensThrottled(id)
	h.mu.Lock()
	h.imageGenCooldownUntil[id] = time.Now().Add(-time.Second)
	h.mu.Unlock()
	if !h.ImageGenAvailable(id) {
		t.Fatal("elapsed daily image cooldown must lift")
	}

	h.MarkImageGenSystemThrottled(id)
	h.mu.Lock()
	h.imageGenSystemCooldown[id] = time.Now().Add(-time.Second)
	h.mu.Unlock()
	if !h.ImageGenAvailable(id) {
		t.Fatal("elapsed system capacity cooldown must lift")
	}
	if _, ok := h.ImageGenCooldownUntil(id); ok {
		t.Fatal("expired cooldown entries must be pruned")
	}
}

func TestImageGenCooldownUntilPrefersLaterDeadline(t *testing.T) {
	h := newAccountHealth()
	const id = "u-1"

	h.MarkImageGenSystemThrottled(id) // 30 minutes
	h.MarkImageGenTokensThrottled(id) // next UTC midnight, always later
	until, ok := h.ImageGenCooldownUntil(id)
	if !ok {
		t.Fatal("cooldown deadline must be reported while throttled")
	}
	if until.Before(time.Now().Add(30 * time.Minute)) {
		t.Fatalf("must report the later of the two deadlines, got %v", until)
	}
}

// A success recorded after the throttle must not clear it. This is the ordering
// the images handler relies on: chatWithAccount marks success for a 200-with-
// refusal, then the handler marks the image quota.
func TestMarkSuccessPreservesImageGenCooldown(t *testing.T) {
	h := newAccountHealth()
	const id = "u-1"

	h.MarkImageGenTokensThrottled(id)
	h.MarkSuccess(id)
	if h.ImageGenAvailable(id) {
		t.Fatal("MarkSuccess must preserve an unexpired image cooldown")
	}
	if !h.Available(id) {
		t.Fatal("MarkSuccess must leave the account usable for chat")
	}
}

// markImageRefusalCooldown is the single place an upstream 200-with-refusal
// turns into an image cooldown. The upstream call itself was a success, so
// chatWithAccount records one either before or after this mark; the invariant
// that makes both orders safe is that MarkSuccess preserves an unexpired image
// cooldown. Pin it through the real entry point, for both refusal kinds.
func TestImageRefusalCooldownSurvivesSuccess(t *testing.T) {
	for _, tc := range []struct {
		name     string
		refusal  imageRefusalKind
		sentinel error
	}{
		{"quota", imageRefusalQuota, errImageQuotaRefused},
		{"capacity", imageRefusalCapacity, errImageServiceUnavailable},
	} {
		t.Run(tc.name+"/mark-then-success", func(t *testing.T) {
			s := &Server{accountPool: newAccountHealth()}
			const id = "u-1"
			if err := s.markImageRefusalCooldown(id, tc.refusal); !errors.Is(err, tc.sentinel) {
				t.Fatalf("markImageRefusalCooldown error = %v, want %v", err, tc.sentinel)
			}
			s.accountPool.MarkSuccess(id)
			if s.accountPool.ImageGenAvailable(id) {
				t.Fatal("image cooldown must survive a MarkSuccess recorded after the mark")
			}
			if !s.accountPool.Available(id) {
				t.Fatal("account must remain in chat rotation")
			}
		})
		t.Run(tc.name+"/success-then-mark", func(t *testing.T) {
			s := &Server{accountPool: newAccountHealth()}
			const id = "u-1"
			s.accountPool.MarkSuccess(id)
			if err := s.markImageRefusalCooldown(id, tc.refusal); !errors.Is(err, tc.sentinel) {
				t.Fatalf("markImageRefusalCooldown error = %v, want %v", err, tc.sentinel)
			}
			if s.accountPool.ImageGenAvailable(id) {
				t.Fatal("image cooldown must survive a MarkSuccess recorded before the mark")
			}
			if !s.accountPool.Available(id) {
				t.Fatal("account must remain in chat rotation")
			}
		})
	}
}

// resolveImageAccount must rotate past image-throttled accounts while
// resolveAccount still hands them out for chat.
func TestResolveImageAccountSkipsImageThrottled(t *testing.T) {
	store := testAccountFiles(t)
	s := &Server{tokens: store, accountPool: newAccountHealth()}

	first, err := s.resolveAccount("")
	if err != nil {
		t.Fatalf("resolve chat account: %v", err)
	}
	s.accountPool.MarkImageGenTokensThrottled(first.ID)

	img, err := s.resolveImageAccount("")
	if err != nil {
		t.Fatalf("resolve image account: %v", err)
	}
	if img.ID == first.ID {
		t.Fatal("image request must rotate off the throttled account")
	}

	// The throttled account is still fine for chat.
	if !s.accountAvailable(first.ID) {
		t.Fatal("image throttle must not remove the account from chat rotation")
	}
}

// With every account throttled, the caller gets 429 plus a Retry-After derived
// from the real cooldown deadline instead of an unbounded probe.
func TestResolveImageAccountAllThrottled(t *testing.T) {
	store := testAccountFiles(t)
	s := &Server{tokens: store, accountPool: newAccountHealth()}
	for _, acc := range store.List() {
		s.accountPool.MarkImageGenSystemThrottled(acc.ID)
	}

	_, err := s.resolveImageAccount("")
	if err == nil {
		t.Fatal("expected an error when every account is image-throttled")
	}
	var httpErr *UpstreamHTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("expected *UpstreamHTTPError, got %T", err)
	}
	if httpErr.Status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", httpErr.Status)
	}
	if httpErr.RetryAfter <= 0 {
		t.Fatalf("RetryAfter = %d, want a positive delay", httpErr.RetryAfter)
	}
}

// An explicitly pinned account bypasses rotation so the caller sees the real
// upstream refusal rather than a silently substituted account.
func TestResolveImageAccountHonoursExplicitPin(t *testing.T) {
	store := testAccountFiles(t)
	s := &Server{tokens: store, accountPool: newAccountHealth()}
	accounts := store.List()
	pinned := accounts[0].ID
	s.accountPool.MarkImageGenTokensThrottled(pinned)

	acc, err := s.resolveImageAccount(pinned)
	if err != nil {
		t.Fatalf("pinned account must resolve: %v", err)
	}
	if acc.ID != pinned {
		t.Fatalf("got account %s, want the pinned %s", acc.ID, pinned)
	}
}

package web

import (
	"testing"
	"time"

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
	// ErrEmptyCompletion belongs here: the upstream produced nothing at all and a
	// retry on a fresh conversation clears it (live-checked 2026-09-02).
	for _, err := range []error{chathub.ErrImageLimit, chathub.ErrMeteringThrottled, chathub.ErrRateLimitNotice, errImageQuotaRefused, errImageServiceUnavailable, chathub.ErrEmptyCompletion} {
		if !imageFailoverWorthwhile(err) {
			t.Fatalf("expected failover for %v", err)
		}
	}
	// A refused attachment and a content-policy block fail identically on every
	// account, so retrying only burns quota.
	for _, err := range []error{nil, chathub.ErrOffensiveContent, chathub.ErrAttachmentRejected} {
		if imageFailoverWorthwhile(err) {
			t.Fatalf("unexpected failover for %v", err)
		}
	}
}

func TestClassifyImageRefusalSeparatesQuotaCapacityAndPolicy(t *testing.T) {
	cases := map[string]imageRefusalKind{
		"Sorry, I can't generate any more images today. Try again tomorrow.":                imageRefusalQuota,
		"今日额度已用完，无法再生成图片":                                                                   imageRefusalQuota,
		"Sorry, the image generation service is currently unavailable. Please try again.":   imageRefusalCapacity,
		"Sorry, the image generation service is currently experiencing unusual demand.":     imageRefusalCapacity,
		"Sorry, I can’t generate images featuring that copyrighted character.":              imageRefusalPolicy,
		"Sorry, I can't generate that image as requested. Try a fully original alternative": imageRefusalPolicy,
		"Sorry, I wasn't able to respond to that. Is there something else I can help with?": imageRefusalUnknown,
		"":                                                                                 imageRefusalUnknown,
	}
	for text, want := range cases {
		if got := classifyImageRefusal(text); got != want {
			t.Fatalf("classify(%q)=%d want %d", text, got, want)
		}
	}
	// The old helper must keep meaning "quota" and nothing else.
	if !isImageQuotaRefusal("no more images today; try again tomorrow") {
		t.Fatal("quota refusal no longer detected")
	}
	if isImageQuotaRefusal("the image generation service is currently unavailable") {
		t.Fatal("capacity outage misreported as a quota refusal")
	}
}

func TestMarkImageLimitedLeavesChatAvailable(t *testing.T) {
	h := newAccountHealth()
	h.MarkImageLimited("account-a")
	if h.ImageGenAvailable("account-a") {
		t.Fatal("image limit did not cool down image generation")
	}
	if !h.Available("account-a") {
		t.Fatal("image limit sidelined the account for chat")
	}
	if !h.ImageLimited("account-a") {
		t.Fatal("dashboard flag not set")
	}
	// A later success must not restore a general cooldown for the image limit.
	h.MarkSuccess("account-a")
	if !h.Available("account-a") {
		t.Fatal("success re-applied a general cooldown for an image limit")
	}
	if h.ImageGenAvailable("account-a") {
		t.Fatal("success cleared the image cooldown that is still in force")
	}
}

func TestDesignerDisabledCoolsImageGenerationOnly(t *testing.T) {
	h := newAccountHealth()
	err := &UpstreamHTTPError{Status: 403, ErrorCode: "ErrorDisallowedAADUser"}
	h.MarkFailure("account-a", err, 60*time.Second)
	if h.ImageGenAvailable("account-a") {
		t.Fatal("Designer-disabled account is still selected for image generation")
	}
	if !h.Available("account-a") {
		t.Fatal("Designer-disabled account lost its chat capacity")
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

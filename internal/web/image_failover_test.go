package web

import (
	"context"
	"fmt"
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
	// errImageNoResource is the same miss wearing prose instead of silence, so it
	// must not be the one case that fails on the first attempt.
	//
	// A WebSocket fault where the socket itself failed before any image is an
	// account-local stall, not a verdict on the prompt: a per-frame read timeout
	// (pings stopped) and an upstream close before completion (classified TCP, e.g.
	// "close 1000 (normal)") both used to surface as a hard 502 on the one
	// half-open account without ever trying another. A bare WS_READ_TIMEOUT with no
	// deadline cause stands in for the pings-stopped socket here.
	for _, err := range []error{
		chathub.ErrImageLimit, chathub.ErrMeteringThrottled, chathub.ErrRateLimitNotice,
		errImageQuotaRefused, errImageServiceUnavailable, chathub.ErrEmptyCompletion, errImageNoResource,
		&chathub.DialError{Status: 0, Kind: "WS_READ_TIMEOUT"},
		&chathub.DialError{Status: 0, Kind: "TCP"},
		fmt.Errorf("chat: %w", &chathub.DialError{Status: 0, Kind: "WS_READ_TIMEOUT"}),
	} {
		if !imageFailoverWorthwhile(err) {
			t.Fatalf("expected failover for %v", err)
		}
	}
	// A refused attachment and a content-policy block fail identically on every
	// account, so retrying only burns quota. A client cancellation is the caller
	// giving up, and a fault after content already streamed cannot be re-issued
	// without duplicating output — neither may rotate.
	//
	// A silent read timeout whose cause is our own image deadline (context
	// deadline exceeded) must NOT rotate: the socket was still alive on keepalive
	// pings and the generation was still running, so failing over abandons a
	// working turn. The live ceiling error is a *chathub.DialError wrapping
	// context.DeadlineExceeded; the plain wrap here exercises the same
	// errors.Is branch.
	for _, err := range []error{
		nil, chathub.ErrOffensiveContent, chathub.ErrAttachmentRejected,
		&chathub.DialError{Status: 0, Kind: "CLIENT_CANCELED"},
		&chathub.DialError{Status: 0, Kind: "WS_READ_TIMEOUT", Streamed: true},
		fmt.Errorf("ws dial: WS_READ_TIMEOUT upstream 0: %w", context.DeadlineExceeded),
	} {
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
		"": imageRefusalUnknown,
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

func TestImageRequestBudgetCapsBelowCallerDeadline(t *testing.T) {
	// The caller's own wall is 300s (GimageUI CLIENT_TIMEOUT_MS and nginx
	// proxy_read_timeout), so the whole-request budget must stay under it or a
	// slow failure races the caller and surfaces as an opaque 499.
	if maxImageRequestBudget >= 300*time.Second {
		t.Fatalf("budget cap %v must stay under the 300s caller deadline", maxImageRequestBudget)
	}
	// The default 150s timeout would grant exactly 300s and the deployed 220s
	// would grant 440s; both must be pulled down to the cap.
	for _, imageTimeout := range []time.Duration{150 * time.Second, 220 * time.Second} {
		if got := imageRequestBudget(imageTimeout); got != maxImageRequestBudget {
			t.Fatalf("timeout %v: budget %v, want the cap %v", imageTimeout, got, maxImageRequestBudget)
		}
	}
	// A short timeout stays well under the cap, so it keeps its full factor —
	// the cap only ever shortens the wait.
	if got := imageRequestBudget(60 * time.Second); got != imageRequestBudgetFactor*60*time.Second {
		t.Fatalf("60s timeout: budget %v, want %v", got, imageRequestBudgetFactor*60*time.Second)
	}
}

func TestImageTimedOutOnlyForReadTimeout(t *testing.T) {
	// Our own read-timeout (the upstream stayed alive on keepalives but never
	// delivered) must surface as a retryable gateway timeout, wrapped or not.
	for _, err := range []error{
		&chathub.DialError{Kind: "WS_READ_TIMEOUT"},
		fmt.Errorf("chat: %w", &chathub.DialError{Kind: "WS_READ_TIMEOUT"}),
		&chathub.DialError{Kind: "WS_READ_TIMEOUT", Streamed: true},
	} {
		if !imageTimedOut(err) {
			t.Fatalf("expected timeout classification for %v", err)
		}
	}
	// Everything else keeps its own shape: an empty completion, a content miss,
	// a client cancel and an upstream close are not "we waited too long".
	for _, err := range []error{
		nil, errImageNoResource, chathub.ErrEmptyCompletion,
		&chathub.DialError{Kind: "CLIENT_CANCELED"},
		&chathub.DialError{Kind: "TCP"},
	} {
		if imageTimedOut(err) {
			t.Fatalf("did not expect timeout classification for %v", err)
		}
	}
}

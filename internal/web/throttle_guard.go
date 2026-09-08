package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"m365-copilot2api/internal/auth"
)

// writeLocalThrottle answers a request locally with a 429 carrying the exact
// remaining cooldown, without touching the upstream. This is the response arm
// of the throttle short-circuit: the account is already marked limited and its
// metering cooldown has not expired, so forwarding the payload would only burn
// quota the account does not have and stretch the upstream throttle window.
func writeLocalThrottle(w http.ResponseWriter, accountID string, remaining time.Duration) {
	retry := int(remaining.Seconds())
	if retry < 1 {
		retry = 1
	}
	until := time.Now().Add(remaining).UTC().Format(time.RFC3339)
	log.Printf("[throttle-shortcircuit] account=%s status=429 cooldown_remaining=%ds cooldown_until=%s", accountID, retry, until)
	RecordThrottleEvent(ThrottleLocalShortCircuit, accountID, fmt.Sprintf("cooldown_remaining=%ds", retry))
	w.Header().Set("Retry-After", fmt.Sprintf("%d", retry))
	w.Header().Set("X-M365-Retry-After", fmt.Sprintf("%d", retry))
	w.Header().Set("X-M365-RateLimit-Reset", fmt.Sprintf("%d", time.Now().Add(remaining).Unix()))
	w.Header().Set("X-M365-RateLimit-Remaining", "0")
	w.Header().Set("X-M365-Cooldown-Until", until)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"type":           "rate_limit_error",
		"code":           "rate_limit_error",
		"message":        "the selected account is rate limited by upstream metering; the request was answered locally without forwarding. Retry after the cooldown.",
		"cooldown_until": until,
	}})
}

// maybeShortCircuitThrottle inspects the resolved account and, when it is
// still inside a quota-429 cooldown, writes the local 429 and reports true.
// It applies to every resolved account — pinned or rotated — because a
// cooldown means the account provably has no upstream budget right now; the
// first request after expiry passes through and acts as the recovery probe.
func (s *Server) maybeShortCircuitThrottle(w http.ResponseWriter, accountID string) bool {
	if s == nil || s.accountPool == nil || accountID == "" {
		return false
	}
	d, ok := s.accountPool.RemainingCooldown(accountID)
	if !ok {
		return false
	}
	writeLocalThrottle(w, accountID, d)
	return true
}

// allowFailoverRequested reports whether the client explicitly authorized
// account failover for this request via the X-M365-Allow-Failover header.
// The pin semantics stay "first attempt goes to the pinned account"; the
// header only lifts the pin's veto over failover, so a client that manages
// its own account rotation (or explicitly prefers availability over affinity)
// no longer has to eat repeated local 429s for an account it did not know was
// dry. Anything other than true/1 keeps the strict pinned behaviour.
func allowFailoverRequested(r *http.Request) bool {
	if r == nil {
		return false
	}
	v := strings.TrimSpace(r.Header.Get("X-M365-Allow-Failover"))
	return strings.EqualFold(v, "true") || v == "1"
}

// handleCooldownGate is the single decision point applied right after account
// resolution on the chat paths. For an account inside its quota cooldown it:
//   - fails over to the next healthy account when the client sent
//     X-M365-Allow-Failover and a healthy alternative exists (acc is updated
//     in place; the request then proceeds normally — no upstream call is
//     burned on the dry pinned account);
//   - otherwise writes the local 429 short-circuit and reports false, meaning
//     the response is already committed and the caller must return.
//
// Reports true when the request may proceed with acc.
func (s *Server) handleCooldownGate(r *http.Request, w http.ResponseWriter, acc *auth.AccountToken) bool {
	if s == nil || s.accountPool == nil || acc == nil || acc.ID == "" {
		return true
	}
	d, ok := s.accountPool.RemainingCooldown(acc.ID)
	if !ok {
		return true
	}
	if allowFailoverRequested(r) {
		if next, nerr := s.nextHealthyAccountExcluding(map[string]bool{acc.ID: true}); nerr == nil {
			log.Printf("[throttle-gate] account=%s in cooldown (%s) → allow-failover to %s", acc.ID, d.Truncate(time.Second), next.ID)
			*acc = next
			return true
		}
		log.Printf("[throttle-gate] account=%s in cooldown (%s), allow-failover requested but no healthy alternative → local 429", acc.ID, d.Truncate(time.Second))
	}
	writeLocalThrottle(w, acc.ID, d)
	return false
}

// requestDebounce replays the verdict of a recently failed identical request
// instead of re-executing it upstream. Client retry storms (exponential
// backoff implemented as tight re-sends of the very same 100KB payload) used
// to re-burn a real upstream call per retry; with the debounce, retries inside
// the window of a 429/5xx verdict are answered from cache. The TTL is fixed
// and NOT refreshed on replay, so one retry slips through roughly every window
// and keeps acting as the recovery probe once the upstream condition clears.
type requestDebounce struct {
	mu      sync.Mutex
	entries map[string]debounceEntry
	ttl     time.Duration
	max     int
}

type debounceEntry struct {
	status  int
	code    string
	message string
	expires time.Time
}

func newRequestDebounce(ttl time.Duration, max int) *requestDebounce {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	if max <= 0 {
		max = 4096
	}
	return &requestDebounce{entries: map[string]debounceEntry{}, ttl: ttl, max: max}
}

func (d *requestDebounce) check(key string) (debounceEntry, bool) {
	if d == nil || key == "" {
		return debounceEntry{}, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.entries[key]
	if !ok {
		return debounceEntry{}, false
	}
	if time.Now().After(e.expires) {
		delete(d.entries, key)
		return debounceEntry{}, false
	}
	return e, true
}

func (d *requestDebounce) rememberFailure(key string, status int, code, message string) {
	if d == nil || key == "" {
		return
	}
	// Only failures that are safe and useful to replay are recorded: 429 and
	// 5xx mean "the upstream said no or broke", which a 30s-later identical
	// retry will almost certainly hit again.
	if status != http.StatusTooManyRequests && status < 500 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.entries) >= d.max {
		now := time.Now()
		for k, e := range d.entries {
			if now.After(e.expires) {
				delete(d.entries, k)
			}
		}
		if len(d.entries) >= d.max {
			// Still full: drop an arbitrary key to keep the map bounded.
			for k := range d.entries {
				delete(d.entries, k)
				break
			}
		}
	}
	d.entries[key] = debounceEntry{status: status, code: code, message: message, expires: time.Now().Add(d.ttl)}
}

// requestFingerprint identifies a retry of the same logical request: tenant,
// conversation (explicit or resolver-resolved), model and the effective
// prompt. The effective answerPrompt is used rather than raw messages so that
// resolver-incremental prompts fingerprint identically to the same request
// retried a second later.
func requestFingerprint(tenant, conversation, model, answerPrompt string) string {
	h := sha256.New()
	_, _ = h.Write([]byte(tenant))
	h.Write([]byte{0})
	_, _ = h.Write([]byte(conversation))
	h.Write([]byte{0})
	_, _ = h.Write([]byte(model))
	h.Write([]byte{0})
	_, _ = h.Write([]byte(answerPrompt))
	return hex.EncodeToString(h.Sum(nil))
}

// debounceReplay answers an in-window retry of a recently failed request from
// the cache and reports true. Fingerprint-less requests pass straight through.
func (s *Server) debounceReplay(w http.ResponseWriter, key, requestID string) bool {
	if s == nil || s.debounce == nil || key == "" {
		return false
	}
	e, hit := s.debounce.check(key)
	if !hit {
		return false
	}
	log.Printf("[debounce] id=%s replay status=%d code=%s", requestID, e.status, e.code)
	RecordThrottleEvent(ThrottleDebounceReplay, "", fmt.Sprintf("status=%d", e.status))
	w.Header().Set("X-M365-Cached-Error", "true")
	if e.status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "30")
	}
	writeOpenAIError(w, e.status, e.code, e.message)
	return true
}

// debounceRememberFailure records the verdict this request is about to hand
// back, so identical retries inside the window replay it locally. Only 429 and
// 5xx verdicts are worth remembering; request-side 4xx errors depend on the
// payload and are not replayed.
func (s *Server) debounceRememberFailure(key string, err error) {
	if s == nil || s.debounce == nil || key == "" || err == nil {
		return
	}
	status := upstreamStatus(err)
	if status != http.StatusTooManyRequests && status < 500 {
		return
	}
	code := "upstream_error"
	if status == http.StatusTooManyRequests {
		code = "rate_limit_error"
	}
	s.debounce.rememberFailure(key, status, code, err.Error())
}

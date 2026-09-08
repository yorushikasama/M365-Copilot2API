package web

import (
	"context"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"m365-copilot2api/internal/chathub"
)

// probeIntervalConfig reads the prober schedule from the environment.
// M365_PROBE_INTERVAL_SECONDS defaults to 60 and is floored at 15 so a
// misconfig cannot turn the prober into a request storm of its own;
// M365_PROBE_ENABLED=false switches the loop off entirely.
func probeIntervalConfig() (time.Duration, bool) {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("M365_PROBE_ENABLED")), "false") {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("M365_PROBE_INTERVAL_SECONDS")))
	if err != nil || n <= 0 {
		n = 60
	}
	if n < 15 {
		n = 15
	}
	return time.Duration(n) * time.Second, true
}

// cooldownProberGuard prevents duplicate in-flight probes of the same account
// (a probe can outlive one ticker period) without holding any accountHealth
// locks across the network call.
type cooldownProberGuard struct {
	mu      sync.Mutex
	running map[string]bool
}

var accountProber = &cooldownProberGuard{running: map[string]bool{}}

func (g *cooldownProberGuard) begin(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.running[id] {
		return false
	}
	g.running[id] = true
	return true
}

func (g *cooldownProberGuard) end(id string) {
	g.mu.Lock()
	delete(g.running, id)
	g.mu.Unlock()
}

// StartCooldownProber launches the background recovery probe loop (the
// new-api channel-test pattern). Metering recovery upstream is silent: the
// gateway only learns an account is usable again when something sends it a
// real request. Without the prober that something is the first unlucky user
// after cooldown expiry; with it, a ~1-token probe keeps recovery invisible
// to clients and warms the pool.
//
// The probe deliberately reuses chatWithAccount so its outcome flows through
// the exact same health accounting (MarkSuccess/MarkFailure, metering
// updates, debounce-irrelevant) as production traffic.
func (s *Server) StartCooldownProber() {
	interval, enabled := probeIntervalConfig()
	if !enabled {
		log.Printf("[probe] disabled via M365_PROBE_ENABLED=false")
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			s.probeExpiredCooldowns()
		}
	}()
	log.Printf("[probe] cooldown prober started interval=%s", interval)
}

// probeExpiredCooldowns runs one tick: snapshot expired limited accounts and
// probe each once. The global circuit is respected — when shared transport is
// shunned, probing every account would only re-confirm the outage and pollute
// the circuit counters.
func (s *Server) probeExpiredCooldowns() {
	if s == nil || s.accountPool == nil || s.tokens == nil || GlobalCircuitIsOpen() {
		return
	}
	candidates := s.accountPool.ExpiredLimitedCooldowns()
	if len(candidates) == 0 {
		return
	}
	// Cap probes per tick: with a healthy pool this is normally 0-2 accounts;
	// after a mass-throttle day the cap bounds the tick's burst and the next
	// ticks pick up the rest (still-failing accounts are re-marked by their
	// own probe failure, so they re-enter candidates only after backoff).
	started := 0
	for id := range candidates {
		if started >= 8 {
			break
		}
		if s.tokens == nil || !s.tokens.ScheduleEnabled(id) {
			continue
		}
		if s.probeAccount(id) {
			started++
		}
	}
}

// probeAccount sends the minimal recovery probe for one account in the
// background and reports whether a probe was started (false = already
// in-flight).
func (s *Server) probeAccount(accountID string) bool {
	if !accountProber.begin(accountID) {
		return false
	}
	go func() {
		defer accountProber.end(accountID)
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		acc, err := s.tokens.EnsureValid(accountID)
		if err != nil {
			log.Printf("[probe] account=%s skipped token_unavailable err=%v", accountID, err)
			return
		}
		cfg := s.settings.get()
		req := chathub.Request{
			// "ping": the smallest payload that still produces a real
			// upstream metering verdict for the chat capability.
			Text:        "ping",
			LicenseType: cfg.LicenseType,
			Scenario:    cfg.Scenario,
			FeatureFlags: s.featureFlags(),
		}
		res, err := s.chatWithAccount(ctx, accountID, chathub.Account{AccessToken: acc.AccessToken, OID: acc.OID, TID: acc.TID}, req)
		if err != nil {
			RecordThrottleEvent(ThrottleProbeFailed, accountID, err.Error())
			log.Printf("[probe] account=%s still_unhealthy err=%v", accountID, err)
			return
		}
		RecordThrottleEvent(ThrottleProbeRecovered, accountID, "")
		log.Printf("[probe] account=%s recovered conversation=%s", accountID, res.ConversationID)
		// The probe runs in a throwaway cloud conversation; drop it so the
		// account's conversation list does not accumulate probe entries (same
		// housekeeping as router turns).
		if res.ConversationID != "" {
			s.dropTransientConversation(res.ConversationID)
		}
	}()
	return true
}

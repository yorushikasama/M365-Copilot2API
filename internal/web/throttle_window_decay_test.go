package web

import (
	"testing"
	"time"
)

// The per-account counters used to be incremented forever and cleared only by
// the manual reset, while the snapshot advertised "windowHours": 24. A storm
// from days earlier therefore kept an account pinned at the top of the admin
// sort and the panel reported a since-boot total as a 24h figure.
func TestThrottleCountersDecayOutOfWindow(t *testing.T) {
	m := newThrottleMetrics(24*time.Hour, 200)
	m.record(ThrottleUpstream429, "acc-old", "storm")

	if got := m.snapshot()["totals"].(accountThrottleCounters).Upstream429; got != 1 {
		t.Fatalf("fresh event should count, got %d", got)
	}

	// Age the recorded bucket past the window without touching the wall clock.
	stale := time.Now().Add(-48 * time.Hour).Truncate(time.Hour)
	m.mu.Lock()
	for i := range m.totals.buckets {
		m.totals.buckets[i].hour = stale
	}
	for _, w := range m.byAccount {
		for i := range w.buckets {
			w.buckets[i].hour = stale
		}
	}
	m.mu.Unlock()

	snap := m.snapshot()
	if got := snap["totals"].(accountThrottleCounters).Upstream429; got != 0 {
		t.Fatalf("counters outside the window must not be reported, got %d", got)
	}
	if rows := snap["accounts"].([]throttleAccountRow); len(rows) != 0 {
		t.Fatalf("fully aged-out account must drop off the panel, got %+v", rows)
	}

	// A new event re-arms the account without resurrecting the stale count.
	m.record(ThrottleUpstream429, "acc-old", "fresh")
	snap = m.snapshot()
	if got := snap["totals"].(accountThrottleCounters).Upstream429; got != 1 {
		t.Fatalf("expected only the fresh event to count, got %d", got)
	}
}

// An account that has been quiet for longer than the window must not keep an
// entry alive in the map.
func TestThrottleAccountMapDropsInactiveAccounts(t *testing.T) {
	m := newThrottleMetrics(24*time.Hour, 200)
	m.record(ThrottleUpstream429, "acc-quiet", "old")

	stale := time.Now().Add(-72 * time.Hour).Truncate(time.Hour)
	m.mu.Lock()
	for _, w := range m.byAccount {
		for i := range w.buckets {
			w.buckets[i].hour = stale
		}
	}
	m.mu.Unlock()

	// Recording for a different account triggers the sweep.
	m.record(ThrottleFailover, "acc-active", "rotate")

	m.mu.Lock()
	_, stillThere := m.byAccount["acc-quiet"]
	m.mu.Unlock()
	if stillThere {
		t.Fatal("inactive account entry should have been swept")
	}
}

package web

import (
	"net/http"
	"sort"
	"sync"
	"time"
)

// Kinds of rate-limit lifecycle events the gateway observes or decides.
const (
	// ThrottleUpstream429: the upstream actually returned a metering/HTTP 429
	// for the account (a real quota burn happened).
	ThrottleUpstream429 = "upstream_429"
	// ThrottleLocalShortCircuit: the gateway answered 429 locally without
	// touching the upstream because the account is inside its cooldown.
	ThrottleLocalShortCircuit = "local_shortcircuit"
	// ThrottleDebounceReplay: an identical in-window retry was answered from
	// the failure cache instead of being re-executed.
	ThrottleDebounceReplay = "debounce_replay"
	// ThrottleFailover: the request moved to a different healthy account.
	ThrottleFailover = "failover"
	// ThrottleProbeRecovered / ThrottleProbeFailed: the background prober's
	// verdict for an account whose cooldown had expired.
	ThrottleProbeRecovered = "probe_recovered"
	ThrottleProbeFailed    = "probe_failed"
)

// throttleEvent is one entry in the rolling event log surfaced on the admin
// 429 panel. Detail carries the error text or the decision rationale; it is
// trimmed of identifiers where they would be noise but keeps account IDs so
// an operator can correlate a storm with the exact account, as the
// 2026-09-08 investigation did from raw journalctl.
type throttleEvent struct {
	At        time.Time `json:"at"`
	Kind      string    `json:"kind"`
	AccountID string    `json:"accountId,omitempty"`
	Detail    string    `json:"detail,omitempty"`
}

// accountThrottleCounters aggregates the rolling-window event counts for one
// account. The admin table sorts by Upstream429 so the dryest account floats
// to the top.
type accountThrottleCounters struct {
	Upstream429       int       `json:"upstream429"`
	LocalShortCircuit int       `json:"localShortCircuit"`
	DebounceReplay    int       `json:"debounceReplay"`
	Failovers         int       `json:"failovers"`
	ProbeRecovered    int       `json:"probeRecovered"`
	ProbeFailed       int       `json:"probeFailed"`
	LastAt            time.Time `json:"lastAt,omitempty"`
}

// throttleMetrics is the in-memory rolling window behind the admin 429 panel.
// It is deliberately a process-global (like globalCircuit): the recording
// hooks live in free functions and Server methods spread across files, and
// plumbing a collector through every call site would double the diff for no
// testability gain — the counters are plain ints under one mutex.
//
// The window is fixed at 24h (the upstream metering day) and the event ring
// is capped; both bound memory regardless of traffic.
// throttleBucket holds one hour of counters. Bucketing is what makes the
// window actually rolling: the counters used to be incremented forever and
// cleared only by the manual reset, so the panel labelled a since-boot total as
// a 24h figure and a storm from days earlier kept an account pinned at the top
// of the sort.
type throttleBucket struct {
	hour     time.Time
	counters accountThrottleCounters
}

// throttleWindow is a short ring of hourly buckets plus the last activity
// timestamp, which is kept outside the buckets so it survives their expiry
// while the entry is still live.
type throttleWindow struct {
	buckets []throttleBucket
	lastAt  time.Time
}

func (w *throttleWindow) bump(kind string, now time.Time, window time.Duration) {
	hour := now.Truncate(time.Hour)
	w.lastAt = now
	cutoff := now.Add(-window).Truncate(time.Hour)
	kept := w.buckets[:0]
	for _, b := range w.buckets {
		if !b.hour.Before(cutoff) {
			kept = append(kept, b)
		}
	}
	w.buckets = kept
	idx := -1
	for i := range w.buckets {
		if w.buckets[i].hour.Equal(hour) {
			idx = i
			break
		}
	}
	if idx < 0 {
		w.buckets = append(w.buckets, throttleBucket{hour: hour})
		idx = len(w.buckets) - 1
	}
	c := &w.buckets[idx].counters
	switch kind {
	case ThrottleUpstream429:
		c.Upstream429++
	case ThrottleLocalShortCircuit:
		c.LocalShortCircuit++
	case ThrottleDebounceReplay:
		c.DebounceReplay++
	case ThrottleFailover:
		c.Failovers++
	case ThrottleProbeRecovered:
		c.ProbeRecovered++
	case ThrottleProbeFailed:
		c.ProbeFailed++
	}
}

// sum totals the buckets still inside the window.
func (w *throttleWindow) sum(now time.Time, window time.Duration) accountThrottleCounters {
	cutoff := now.Add(-window).Truncate(time.Hour)
	var out accountThrottleCounters
	for _, b := range w.buckets {
		if b.hour.Before(cutoff) {
			continue
		}
		out.Upstream429 += b.counters.Upstream429
		out.LocalShortCircuit += b.counters.LocalShortCircuit
		out.DebounceReplay += b.counters.DebounceReplay
		out.Failovers += b.counters.Failovers
		out.ProbeRecovered += b.counters.ProbeRecovered
		out.ProbeFailed += b.counters.ProbeFailed
	}
	out.LastAt = w.lastAt
	return out
}

// empty reports whether every bucket has aged out, so the account entry can be
// dropped and the map stays bounded by *active* accounts.
func (w *throttleWindow) empty(now time.Time, window time.Duration) bool {
	cutoff := now.Add(-window).Truncate(time.Hour)
	for _, b := range w.buckets {
		if !b.hour.Before(cutoff) {
			return false
		}
	}
	return true
}

type throttleMetrics struct {
	mu        sync.Mutex
	window    time.Duration
	maxEvents int
	startedAt time.Time
	events    []throttleEvent
	byAccount map[string]*throttleWindow
	totals    throttleWindow
}

var throttleMetricsSingleton = newThrottleMetrics(24*time.Hour, 200)

func newThrottleMetrics(window time.Duration, maxEvents int) *throttleMetrics {
	return &throttleMetrics{
		window:    window,
		maxEvents: maxEvents,
		startedAt: time.Now(),
		byAccount: map[string]*throttleWindow{},
	}
}

func (t *throttleMetrics) record(kind, accountID, detail string) {
	if t == nil || kind == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	// Prune the event ring first so appends stay bounded even under a storm.
	cutoff := now.Add(-t.window)
	kept := t.events[:0]
	for _, e := range t.events {
		if e.At.After(cutoff) {
			kept = append(kept, e)
		}
	}
	t.events = kept
	if len(t.events) >= t.maxEvents {
		// Drop the oldest half rather than one entry: storm bursts then evict
		// in coarse steps instead of churning every append.
		t.events = append([]throttleEvent(nil), t.events[len(t.events)/2:]...)
	}
	t.events = append(t.events, throttleEvent{At: now, Kind: kind, AccountID: accountID, Detail: detail})

	t.totals.bump(kind, now, t.window)
	if accountID != "" {
		c, ok := t.byAccount[accountID]
		if !ok {
			c = &throttleWindow{}
			t.byAccount[accountID] = c
		}
		c.bump(kind, now, t.window)
	}
	// Drop accounts whose whole window has aged out so the map is bounded by
	// active accounts rather than every account ever seen.
	for id, w := range t.byAccount {
		if id != accountID && w.empty(now, t.window) {
			delete(t.byAccount, id)
		}
	}
}

// throttleAccountRow is one per-account line of the snapshot. The counters
// struct is embedded so its json tags marshal inline.
type throttleAccountRow struct {
	AccountID string `json:"accountId"`
	accountThrottleCounters
}

// snapshot renders the current window. Events are returned newest-first and
// capped at 100 so the panel payload stays small; per-account counters are
// sorted by upstream-429 count descending.
func (t *throttleMetrics) snapshot() map[string]any {
	if t == nil {
		return map[string]any{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	events := make([]throttleEvent, 0, len(t.events))
	for i := len(t.events) - 1; i >= 0 && len(events) < 100; i-- {
		events = append(events, t.events[i])
	}
	now := time.Now()
	rows := make([]throttleAccountRow, 0, len(t.byAccount))
	for id, c := range t.byAccount {
		if c.empty(now, t.window) {
			continue
		}
		rows = append(rows, throttleAccountRow{AccountID: id, accountThrottleCounters: c.sum(now, t.window)})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Upstream429 != rows[j].Upstream429 {
			return rows[i].Upstream429 > rows[j].Upstream429
		}
		return rows[i].LastAt.After(rows[j].LastAt)
	})
	return map[string]any{
		"windowHours": int(t.window.Hours()),
		"startedAt":   t.startedAt,
		"totals":      t.totals.sum(now, t.window),
		"accounts":    rows,
		"events":      events,
	}
}

func (t *throttleMetrics) reset() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.startedAt = time.Now()
	t.events = nil
	t.byAccount = map[string]*throttleWindow{}
	t.totals = throttleWindow{}
}

// RecordThrottleEvent is the recording hook for the whole rate-limit chain.
// Safe on a nil registry for the same reason globalCircuit guards are: the
// hooks must never be able to break the request path they observe.
func RecordThrottleEvent(kind, accountID, detail string) {
	throttleMetricsSingleton.record(kind, accountID, detail)
}

// throttleStats serves the admin 429 panel: GET returns the rolling snapshot,
// DELETE clears it (an operator action after reviewing a storm).
func (s *Server) throttleStats(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		jsonOut(w, throttleMetricsSingleton.snapshot())
	case http.MethodDelete:
		throttleMetricsSingleton.reset()
		jsonOut(w, map[string]any{"status": "ok"})
	default:
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
	}
}

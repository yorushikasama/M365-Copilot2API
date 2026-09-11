package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A failed save must leave the in-memory rule set untouched. Splicing the slice
// in place clobbered the backing array, so the rule stayed unblocked in memory
// while the file still listed it, and the block reappeared on the next restart.
func TestIPManagerRemoveKeepsRuleWhenSaveFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rules.json")
	m := &ipManager{path: path}
	keep, err := m.add("198.51.100.0/24", "keep")
	if err != nil {
		t.Fatal(err)
	}
	drop, err := m.add("203.0.113.7", "drop")
	if err != nil {
		t.Fatal(err)
	}

	// Point the store at a path that cannot be written so saveLocked fails.
	m.path = filepath.Join(path, "unwritable", "rules.json")
	if err := m.remove(drop.ID); err == nil {
		t.Fatal("remove reported success despite an unwritable rules file")
	}
	if !m.blocked("203.0.113.7") {
		t.Fatal("rule was dropped from memory even though the save failed")
	}
	if !m.blocked("198.51.100.42") {
		t.Fatal("unrelated rule was corrupted by the failed remove")
	}
	rules := m.list()
	if len(rules) != 2 {
		t.Fatalf("len(rules) = %d, want 2", len(rules))
	}
	if rules[0].ID != keep.ID || rules[1].ID != drop.ID {
		t.Fatalf("rule order/identity changed: %+v", rules)
	}

	// A subsequent successful remove must still work.
	m.path = path
	if err := m.remove(drop.ID); err != nil {
		t.Fatal(err)
	}
	if m.blocked("203.0.113.7") || !m.blocked("198.51.100.42") {
		t.Fatal("successful remove did not take effect correctly")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("rules file missing after remove: %v", err)
	}
}

// The handler must not annotate the cached snapshot maps in place: they are
// shared by every request inside the snapshot TTL, so one request's block
// verdict leaked into later responses (and raced concurrent readers).
func TestIPManagementDoesNotMutateCachedSnapshot(t *testing.T) {
	dir := t.TempDir()
	usage := &usageLog{Path: filepath.Join(dir, "usage.jsonl")}
	usage.persist = &persistStore{flush: usage.flush}
	now := time.Now()
	usage.records = []UsageRecord{
		{Time: now, ClientIP: "203.0.113.7", InputTokens: 3},
		{Time: now, ClientIP: "198.51.100.9", InputTokens: 5},
	}
	s := &Server{usage: usage, ipManager: &ipManager{path: filepath.Join(dir, "rules.json")}}
	if _, err := s.ipManager.add("203.0.113.7", "blocked"); err != nil {
		t.Fatal(err)
	}

	blockedCount := func() int {
		req := httptest.NewRequest(http.MethodGet, "/api/admin/ip-management?days=7", nil)
		rec := httptest.NewRecorder()
		s.ipManagement(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var body struct {
			IPs []struct {
				IP      string `json:"ip"`
				Blocked bool   `json:"blocked"`
			} `json:"ips"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if len(body.IPs) != 2 {
			t.Fatalf("len(ips) = %d, want 2", len(body.IPs))
		}
		n := 0
		for _, row := range body.IPs {
			if row.Blocked {
				n++
			}
		}
		return n
	}

	if got := blockedCount(); got != 1 {
		t.Fatalf("blocked rows = %d, want 1", got)
	}

	// Unblock and re-query within the snapshot TTL. If the first response had
	// written into the cached maps, the stale verdict would survive here.
	for _, r := range s.ipManager.list() {
		if err := s.ipManager.remove(r.ID); err != nil {
			t.Fatal(err)
		}
	}
	if got := blockedCount(); got != 0 {
		t.Fatalf("blocked rows after unblocking = %d, want 0 (cached snapshot was mutated)", got)
	}

	// The cached snapshot itself must carry no handler-added keys.
	for _, item := range usage.ipSnapshot(7) {
		if _, ok := item["blocked"]; ok {
			t.Fatal("cached snapshot row was annotated with \"blocked\"")
		}
		if _, ok := item["matchedRule"]; ok {
			t.Fatal("cached snapshot row was annotated with \"matchedRule\"")
		}
	}
}

// Each aggregation keeps its own freshness stamp. With one shared stamp, polling
// the stats view kept renewing it and the IP view was served from an entry that
// never aged out on its own.
func TestUsageSnapshotViewsExpireIndependently(t *testing.T) {
	usage := &usageLog{Path: filepath.Join(t.TempDir(), "usage.jsonl")}
	usage.persist = &persistStore{flush: usage.flush}
	usage.records = []UsageRecord{{Time: time.Now(), ClientIP: "203.0.113.7", InputTokens: 1}}

	first := usage.ipSnapshot(7)
	if len(first) != 1 {
		t.Fatalf("len(ips) = %d, want 1", len(first))
	}

	// Age the IP view past the TTL while leaving the stats view fresh, the way a
	// console that polls stats far more often than the IP page would.
	usage.cacheMu.Lock()
	usage.cache.ipsAt = time.Now().Add(-2 * usageSnapshotTTL)
	usage.cacheMu.Unlock()
	usage.snapshot(7)

	usage.mu.Lock()
	usage.records = append(usage.records, UsageRecord{Time: time.Now(), ClientIP: "198.51.100.9", InputTokens: 1})
	usage.mu.Unlock()

	second := usage.ipSnapshot(7)
	if len(second) != 2 {
		t.Fatalf("len(ips) = %d, want 2: the expired IP view was served from cache", len(second))
	}
}

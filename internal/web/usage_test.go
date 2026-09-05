package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestUsageLog(t *testing.T) *usageLog {
	t.Helper()
	s := &usageLog{Path: filepath.Join(t.TempDir(), "usage.jsonl")}
	s.persist = &persistStore{flush: s.flush}
	return s
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	trimmed := strings.TrimRight(string(b), "\n")
	if trimmed == "" {
		return 0
	}
	return strings.Count(trimmed, "\n") + 1
}

// usage.jsonl had no rotation at all: trim() only shrank the in-memory slice
// while the file grew forever and every restart re-scanned all of it. Rotation
// can only reclaim rows that already fell out of the retained window, which is
// what this test sets up.
func TestUsageLogRotatesOversizedFile(t *testing.T) {
	t.Setenv("M365_USAGE_LOG_MAX_BYTES", "2048")
	s := newTestUsageLog(t)

	for i := 0; i < 200; i++ {
		s.record(UsageRecord{Time: time.Now(), Model: "gpt-5", Endpoint: "/v1/chat/completions", InputTokens: 10, OutputTokens: 20})
	}
	if err := s.flush(); err != nil {
		t.Fatal(err)
	}
	if got := countLines(t, s.Path); got != 200 {
		t.Fatalf("%d rows on disk before rotation, want 200", got)
	}
	before, err := os.Stat(s.Path)
	if err != nil {
		t.Fatal(err)
	}

	// Stand in for trim(): the retained window slid forward, so most of the file
	// is now dead weight.
	s.mu.Lock()
	s.records = s.records[160:]
	s.mu.Unlock()

	if err := s.rotateIfNeeded(); err != nil {
		t.Fatal(err)
	}
	if s.rotations != 1 {
		t.Fatalf("rotations = %d, want 1", s.rotations)
	}
	// Rotation must preserve the retained records, not truncate to nothing.
	if got := countLines(t, s.Path); got != 40 {
		t.Fatalf("rotated file holds %d records, want 40", got)
	}
	if s.fileRecords != 40 {
		t.Fatalf("fileRecords = %d, want 40 after rotation", s.fileRecords)
	}
	after, err := os.Stat(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	// The byte cap is a rotation trigger, not a hard ceiling: the retained record
	// count is the real floor, so assert the file actually shrank.
	if after.Size() >= before.Size() {
		t.Fatalf("file did not shrink: %d -> %d bytes", before.Size(), after.Size())
	}
}

// Every row on disk is still retained in memory, so a rewrite cannot shrink the
// file. The byte cap alone must not trigger one, otherwise each 5s flush past the
// cap would rewrite byte-identical content forever.
func TestUsageLogSkipsRotationWithNothingToReclaim(t *testing.T) {
	t.Setenv("M365_USAGE_LOG_MAX_BYTES", "1")
	s := newTestUsageLog(t)

	for i := 0; i < 50; i++ {
		s.record(UsageRecord{Time: time.Now(), Model: "gpt-5", Endpoint: "/v1/messages"})
	}
	for i := 0; i < 4; i++ {
		if err := s.flush(); err != nil {
			t.Fatal(err)
		}
	}
	if s.rotations != 0 {
		t.Fatalf("rotations = %d, want 0: nothing on disk is reclaimable", s.rotations)
	}
	if got := countLines(t, s.Path); got != 50 {
		t.Fatalf("file holds %d rows, want 50", got)
	}
}

// Rotation clears the pending queue in the same critical section it snapshots,
// so a subsequent flush must not append the same rows a second time.
func TestUsageLogRotationDoesNotDuplicateRecords(t *testing.T) {
	t.Setenv("M365_USAGE_LOG_MAX_BYTES", "1024")
	s := newTestUsageLog(t)

	for i := 0; i < 120; i++ {
		s.record(UsageRecord{Time: time.Now(), Model: "gpt-5", Endpoint: "/v1/messages"})
	}
	if err := s.flush(); err != nil {
		t.Fatal(err)
	}
	// Give rotation something to reclaim, then queue a row it has to fold into
	// its own rewrite.
	s.mu.Lock()
	s.records = s.records[100:]
	s.mu.Unlock()
	s.record(UsageRecord{Time: time.Now(), Model: "gpt-5", Endpoint: "/v1/messages"})

	if err := s.rotateIfNeeded(); err != nil {
		t.Fatal(err)
	}
	if s.rotations != 1 {
		t.Fatalf("rotations = %d, want 1", s.rotations)
	}
	// 20 retained rows plus the one recorded just before rotation.
	if got := countLines(t, s.Path); got != 21 {
		t.Fatalf("file holds %d records after rotation, want 21", got)
	}
	if err := s.flush(); err != nil {
		t.Fatal(err)
	}
	if got := countLines(t, s.Path); got != 21 {
		t.Fatalf("file holds %d records after a second flush, want 21", got)
	}
}

// A rotated file must be reloadable, and load() must keep the newest rows.
func TestUsageLogLoadKeepsNewestRecords(t *testing.T) {
	s := newTestUsageLog(t)
	base := time.Now().Add(-time.Hour).Truncate(time.Second)

	var buf []byte
	total := maxUsageRecords + 25
	for i := 0; i < total; i++ {
		b, err := json.Marshal(UsageRecord{Time: base.Add(time.Duration(i) * time.Millisecond), Model: "gpt-5", InputTokens: int64(i)})
		if err != nil {
			t.Fatal(err)
		}
		buf = append(buf, b...)
		buf = append(buf, '\n')
	}
	if err := os.WriteFile(s.Path, buf, 0600); err != nil {
		t.Fatal(err)
	}

	s.load()
	if len(s.records) != maxUsageRecords {
		t.Fatalf("loaded %d records, want %d", len(s.records), maxUsageRecords)
	}
	// The ring buffer must hand back the tail in chronological order.
	if got := s.records[0].InputTokens; got != int64(total-maxUsageRecords) {
		t.Fatalf("first retained record InputTokens = %d, want %d", got, total-maxUsageRecords)
	}
	if got := s.records[len(s.records)-1].InputTokens; got != int64(total-1) {
		t.Fatalf("last retained record InputTokens = %d, want %d", got, total-1)
	}
}

// Recording new usage must invalidate the aggregate cache, otherwise the console
// would keep showing stale totals.
func TestUsageSnapshotCacheInvalidatesOnRecord(t *testing.T) {
	s := newTestUsageLog(t)
	s.record(UsageRecord{Time: time.Now(), Model: "gpt-5", InputTokens: 5})

	first := s.snapshot(7)
	firstReqs := first["summary"].(map[string]any)["requests"].(int64)
	if firstReqs != 1 {
		t.Fatalf("requests = %d, want 1", firstReqs)
	}
	if s.snapshot(7) == nil {
		t.Fatal("cached snapshot must be reusable")
	}

	s.record(UsageRecord{Time: time.Now(), Model: "gpt-5", InputTokens: 5})
	second := s.snapshot(7)
	if got := second["summary"].(map[string]any)["requests"].(int64); got != 2 {
		t.Fatalf("requests = %d, want 2 after a new record invalidated the cache", got)
	}
}

// logs() must return only the requested page.
func TestUsageLogsReturnsRequestedPage(t *testing.T) {
	s := newTestUsageLog(t)
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 100; i++ {
		s.record(UsageRecord{Time: base.Add(time.Duration(i) * time.Second), InputTokens: int64(i)})
	}
	page := s.logs(10, 0)
	if got := page["total"].(int); got != 100 {
		t.Fatalf("total = %d, want 100", got)
	}
	rows := page["logs"].([]UsageRecord)
	if len(rows) != 10 {
		t.Fatalf("returned %d rows, want 10", len(rows))
	}
	// Newest first.
	if rows[0].InputTokens != 99 {
		t.Fatalf("newest row InputTokens = %d, want 99", rows[0].InputTokens)
	}
}

package web

import (
	"testing"
	"time"
)

// today_requests used to be computed with Truncate(24*time.Hour), which rounds
// down against the zero time in UTC and ignores the location. In a UTC+8
// deployment that put the boundary at 08:00 local, so the first eight hours of
// the local day were dropped from the summary while the trend chart (which uses
// the local date) still counted them.
func TestTodaySummaryUsesLocalMidnight(t *testing.T) {
	saved := time.Local
	time.Local = time.FixedZone("TEST+8", 8*3600)
	defer func() { time.Local = saved }()

	now := time.Now()
	localMidnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	if !now.After(localMidnight.Add(time.Minute)) {
		t.Skip("run is within a minute of local midnight; boundary case is ambiguous")
	}

	u := &usageLog{}
	u.records = []UsageRecord{{
		Time:         localMidnight.Add(time.Minute),
		Endpoint:     "/v1/chat/completions",
		Model:        "m365-copilot",
		InputTokens:  10,
		OutputTokens: 5,
		Status:       200,
	}}

	summary := u.computeSnapshot(30)["summary"].(map[string]any)
	if got := summary["today_requests"].(int64); got != 1 {
		t.Fatalf("a record one minute after local midnight must count as today, got today_requests=%d", got)
	}
	if got := summary["today_tokens"].(int64); got != 15 {
		t.Fatalf("today_tokens=%d want 15", got)
	}
}

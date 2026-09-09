package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"m365-copilot2api/internal/chathub"
)

func throttlingPayload(capability string, remaining int) map[string]any {
	return map[string]any{
		"metering": map[string]any{
			capability: map[string]any{"remainingAllowance": remaining},
		},
	}
}

func TestChatQuotaExhausted(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain-429", &UpstreamHTTPError{Status: 429}, false},
		{"metering-no-throttling", &chathub.MeteringError{Cause: chathub.ErrMeteringThrottled}, false},
		{"llm-zero", &chathub.MeteringError{Cause: chathub.ErrMeteringThrottled, Throttling: throttlingPayload("LLMOnly", 0)}, true},
		{"llm-headroom", &chathub.MeteringError{Cause: chathub.ErrMeteringThrottled, Throttling: throttlingPayload("LLMOnly", 3)}, false},
		{"image-only-zero", &chathub.MeteringError{Cause: chathub.ErrMeteringThrottled, Throttling: throttlingPayload("ImageGeneration", 0)}, false},
		{"llm-and-image-zero", &chathub.MeteringError{Cause: chathub.ErrMeteringThrottled, Throttling: throttlingPayload("LLMOnly", 0), Metering: throttlingPayload("ImageGeneration", 0)}, true},
	}
	for _, tc := range cases {
		if got := chatQuotaExhausted(tc.err); got != tc.want {
			t.Errorf("%s: chatQuotaExhausted=%v want %v", tc.name, got, tc.want)
		}
	}
}

func TestMarkFailureExhaustedMeteringCoolsToMidnight(t *testing.T) {
	h := newAccountHealth()
	// Exhausted chat allowance: cooldown must reach (roughly) the next UTC
	// midnight instead of the 30s→30min exponential ladder.
	exhausted := &chathub.MeteringError{Cause: chathub.ErrMeteringThrottled, Throttling: throttlingPayload("LLMOnly", 0)}
	h.MarkFailure("acc", exhausted, 60*time.Second)
	until, ok := h.CooldownUntil("acc")
	if !ok {
		t.Fatal("cooldown must be recorded")
	}
	// Compare against the actual next UTC midnight rather than a fixed
	// duration: a hardcoded ">12h" only holds before 12:00 UTC and fails
	// for the rest of the day.
	expected := nextUTCMidnight()
	if delta := until.Sub(expected); delta < -time.Second || delta > time.Second {
		t.Fatalf("exhausted cooldown until=%v, want next UTC midnight %v", until, expected)
	}
	if !h.RateLimited("acc") {
		t.Fatal("account must be marked limited")
	}
	if d, ok := h.RemainingCooldown("acc"); !ok || d <= 0 {
		t.Fatalf("RemainingCooldown=%v,%v want positive", d, ok)
	}

	// Ordinary 429 without metering evidence keeps the short exponential cooldown.
	h2 := newAccountHealth()
	h2.MarkFailure("acc", &UpstreamHTTPError{Status: 429}, 60*time.Second)
	until2, ok := h2.CooldownUntil("acc")
	if !ok {
		t.Fatal("cooldown must be recorded")
	}
	if d := time.Until(until2); d > 35*time.Minute {
		t.Fatalf("plain 429 cooldown=%v, want <=30min", d)
	}

	// Image-side throttle must not cool chat to midnight.
	h3 := newAccountHealth()
	h3.MarkFailure("acc", &chathub.MeteringError{Cause: chathub.ErrMeteringThrottled, Throttling: throttlingPayload("ImageGeneration", 0)}, 60*time.Second)
	until3, _ := h3.CooldownUntil("acc")
	if d := time.Until(until3); d > 35*time.Minute {
		t.Fatalf("image-only throttle cooldown=%v, want <=30min", d)
	}
}

func TestRemainingCooldownExpires(t *testing.T) {
	h := newAccountHealth()
	h.mu.Lock()
	h.limited["acc"] = true
	h.cooldown["acc"] = time.Now().Add(-time.Second)
	h.mu.Unlock()
	if _, ok := h.RemainingCooldown("acc"); ok {
		t.Fatal("expired cooldown must not short-circuit")
	}
	if _, ok := h.RemainingCooldown("unknown"); ok {
		t.Fatal("unknown account must not short-circuit")
	}
}

func TestMaybeShortCircuitThrottle(t *testing.T) {
	h := newAccountHealth()
	s := &Server{accountPool: h, settings: &settingsStore{v: defaultRuntimeSettings()}}
	// Healthy account: no short-circuit.
	if s.maybeShortCircuitThrottle(httptest.NewRecorder(), "acc") {
		t.Fatal("healthy account must not short-circuit")
	}
	h.MarkFailure("acc", &UpstreamHTTPError{Status: 429}, 10*time.Minute)
	rec := httptest.NewRecorder()
	if !s.maybeShortCircuitThrottle(rec, "acc") {
		t.Fatal("limited account must short-circuit")
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got == "" || got == "0" {
		t.Fatalf("Retry-After=%q want positive seconds", got)
	}
	if rec.Header().Get("X-M365-Cooldown-Until") == "" {
		t.Fatal("X-M365-Cooldown-Until header missing")
	}
	if !strings.Contains(rec.Body.String(), "rate_limit_error") {
		t.Fatalf("body=%q want rate_limit_error code", rec.Body.String())
	}
}

func TestChatAvailableSkipsExhaustedEstimate(t *testing.T) {
	h := newAccountHealth()
	// No observation at all: available.
	if !h.ChatAvailable("acc") {
		t.Fatal("unobserved account must be chat-available")
	}
	h.UpdateMetering("acc", "", true, map[string]int{"LLMOnly": 1})
	if !h.ChatAvailable("acc") {
		t.Fatal("headroom account must be chat-available")
	}
	h.RecordAllowanceConsumption("acc", "LLMOnly")
	if h.ChatAvailable("acc") {
		t.Fatal("exhausted estimate must make the account chat-unavailable")
	}
	// Estimates ratchet down, so a same-day fresh observation with headroom
	// does not raise the estimate back: the account stays sidelined until the
	// metering day rolls over.
	h.UpdateMetering("acc", "", true, map[string]int{"LLMOnly": 5})
	if h.ChatAvailable("acc") {
		t.Fatal("same-day observation must not raise a ratcheted-down estimate")
	}
	h.allowanceUpdatedAt["acc"] = time.Now().UTC().AddDate(0, 0, -1)
	if !h.ChatAvailable("acc") {
		t.Fatal("stale (previous UTC day) exhausted estimate must be dropped")
	}
}

func TestResolveAccountRotationSkipsChatExhausted(t *testing.T) {
	store := testAccountFiles(t)
	s := &Server{tokens: store, accountPool: newAccountHealth()}
	s.accountPool.UpdateMetering("u-1", "", true, map[string]int{"LLMOnly": 1})
	s.accountPool.RecordAllowanceConsumption("u-1", "LLMOnly")
	for i := 0; i < 3; i++ {
		acc, err := s.resolveAccount("")
		if err != nil {
			t.Fatalf("resolveAccount: %v", err)
		}
		if acc.ID == "u-1" {
			t.Fatalf("rotation picked chat-exhausted account u-1 (iteration %d)", i)
		}
	}
}

func TestRequestDebounce(t *testing.T) {
	d := newRequestDebounce(60*time.Millisecond, 16)
	key := requestFingerprint("tenant", "conv", "model", "prompt")
	if e, hit := d.check(key); hit {
		t.Fatalf("empty cache hit: %+v", e)
	}
	d.rememberFailure(key, http.StatusTooManyRequests, "rate_limit_error", "throttled")
	e, hit := d.check(key)
	if !hit || e.status != http.StatusTooManyRequests {
		t.Fatalf("want cached 429, got hit=%v entry=%+v", hit, e)
	}
	// Request-side 4xx are not cached.
	d.rememberFailure(requestFingerprint("t", "c", "m", "p2"), http.StatusBadRequest, "bad", "no")
	if _, hit := d.check(requestFingerprint("t", "c", "m", "p2")); hit {
		t.Fatal("400 must not be cached")
	}
	// TTL expiry lets the next request through as a recovery probe.
	time.Sleep(80 * time.Millisecond)
	if _, hit := d.check(key); hit {
		t.Fatal("entry must expire")
	}
}

func TestRouterWindowMessages(t *testing.T) {
	msgs := []oaiMsg{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "u1"},
		{Role: "assistant", Content: "a", ToolCalls: []map[string]any{{"id": "c1"}}},
		{Role: "tool", Content: "tool result", ToolCallID: "c1"},
		{Role: "user", Content: "u2"},
		{Role: "user", Content: "u3"},
	}
	window := routerWindowMessages(msgs, 2)
	// system always kept; the last two non-system messages follow it. The
	// window start (u2) is not a tool result, so no orphan walk happens.
	if window[0].Role != "system" {
		t.Fatalf("first window role=%s want system", window[0].Role)
	}
	if len(window) != 3 { // sys + u2 + u3
		t.Fatalf("window len=%d want 3", len(window))
	}
	// Orphan case: tail=1 lands on the tool result → walk back to its assistant.
	orphan := []oaiMsg{
		{Role: "user", Content: "u1"},
		{Role: "assistant", Content: "a", ToolCalls: []map[string]any{{"id": "c1"}}},
		{Role: "tool", Content: "r", ToolCallID: "c1"},
	}
	w2 := routerWindowMessages(orphan, 1)
	if w2[0].Role != "assistant" {
		t.Fatalf("window must include the assistant tool_calls owner, got %s", w2[0].Role)
	}
}

func TestRouterWindowNeedsFullHistory(t *testing.T) {
	window := []oaiMsg{{Role: "user", Content: "继续用刚才那个工具查一下"}}
	if !routerWindowNeedsFullHistory(window) {
		t.Fatal("deictic input must upgrade to full history")
	}
	window = []oaiMsg{{Role: "user", Content: "帮我查一下北京明天天气"}}
	if routerWindowNeedsFullHistory(window) {
		t.Fatal("plain input must not upgrade")
	}
}

func TestTrimRouterWindowHead(t *testing.T) {
	s := strings.Repeat("a", 100) + "\n[user]\n" + strings.Repeat("b", 100)
	got := trimRouterWindowHead(s, 120)
	if len(got) > 120+len("[earlier history trimmed for routing]\n") {
		t.Fatalf("trim produced %d bytes over budget", len(got))
	}
	if !strings.HasSuffix(got, strings.Repeat("b", 100)) {
		t.Fatal("trim must keep the tail (most recent messages)")
	}
	if trimRouterWindowHead(s, 1000) != s {
		t.Fatal("under budget the string is untouched")
	}
}

func TestBuildRoutePromptSlimming(t *testing.T) {
	big := strings.Repeat("历史内容占位文本。", 400) // ~10KB per message
	msgs := []oaiMsg{{Role: "system", Content: "sys"}}
	for i := 0; i < 20; i++ {
		msgs = append(msgs, oaiMsg{Role: "user", Content: big})
	}
	toolMaps := []map[string]any{{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{}}}}
	cfg := defaultRuntimeSettings()
	s := &Server{settings: &settingsStore{v: cfg}}
	body := &oaiReq{Messages: msgs, ToolChoice: "auto"}
	flatAll, _ := flattenPromptMessages(msgs, nil)

	// Legacy behaviour (slimming off) routes on the full prompt.
	cfgOff := defaultRuntimeSettings()
	cfgOff.RouterPromptSlimming = false
	sOff := &Server{settings: &settingsStore{v: cfgOff}}
	full := sOff.buildRoutePrompt(body, flatAll, flatAll, agentLedger{}, "", toolMaps)
	if !strings.Contains(full, big) {
		t.Fatal("slimming off must keep the full prompt")
	}

	// Slimming on: prompt shrinks but keeps the most recent message.
	slim := s.buildRoutePrompt(body, flatAll, flatAll, agentLedger{}, "", toolMaps)
	if len(slim) >= len(full) {
		t.Fatalf("slim=%d must be smaller than full=%d", len(slim), len(full))
	}
	if !strings.Contains(slim, big) {
		t.Fatal("slim prompt must still contain the most recent message")
	}

	// Deictic input in the recent window upgrades to the full prompt.
	deictic := msgs[:len(msgs)-1]
	deictic = append(deictic, oaiMsg{Role: "user", Content: "继续用上面第一个工具"})
	bodyDeictic := &oaiReq{Messages: deictic, ToolChoice: "auto"}
	upgraded := s.buildRoutePrompt(bodyDeictic, flatAll, flatAll, agentLedger{}, "", toolMaps)
	if len(upgraded) < len(full) {
		t.Fatalf("deictic routing must use the full prompt (%d < %d)", len(upgraded), len(full))
	}
}

func TestRouterFailoverCapSetting(t *testing.T) {
	cfg := defaultRuntimeSettings()
	if cfg.FailoverMaxAttempts != 3 {
		t.Fatalf("default FailoverMaxAttempts=%d want 3", cfg.FailoverMaxAttempts)
	}
	if !cfg.RouterPromptSlimming {
		t.Fatal("router prompt slimming must default to enabled")
	}
	if cfg.RouterPromptTailMessages != 4 || cfg.RouterPromptMaxBytes != 4096 {
		t.Fatalf("slimming defaults tail=%d bytes=%d want 4/4096", cfg.RouterPromptTailMessages, cfg.RouterPromptMaxBytes)
	}
}

func TestAllowFailoverRequested(t *testing.T) {
	cases := []struct {
		header string
		want   bool
	}{
		{"", false},
		{"true", true},
		{"True", true},
		{"1", true},
		{"yes", false},
		{"false", false},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		if tc.header != "" {
			r.Header.Set("X-M365-Allow-Failover", tc.header)
		}
		if got := allowFailoverRequested(r); got != tc.want {
			t.Errorf("header=%q allowFailoverRequested=%v want %v", tc.header, got, tc.want)
		}
	}
	if allowFailoverRequested(nil) {
		t.Fatal("nil request must not allow failover")
	}
}

func TestHandleCooldownGate(t *testing.T) {
	store := testAccountFiles(t)
	h := newAccountHealth()
	s := &Server{tokens: store, accountPool: h, settings: &settingsStore{v: defaultRuntimeSettings()}}

	// Healthy account: gate passes through untouched.
	acc, err := store.EnsureValid("u-1")
	if err != nil {
		t.Fatalf("EnsureValid: %v", err)
	}
	rec := httptest.NewRecorder()
	if !s.handleCooldownGate(httptest.NewRequest(http.MethodPost, "/", nil), rec, &acc) {
		t.Fatal("healthy account must pass the gate")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("healthy gate wrote status %d", rec.Code)
	}

	// Cooldown without the header: local 429, account unchanged.
	h.MarkFailure("u-1", &UpstreamHTTPError{Status: 429}, 10*time.Minute)
	pinned, _ := store.EnsureValid("u-1")
	rec = httptest.NewRecorder()
	if s.handleCooldownGate(httptest.NewRequest(http.MethodPost, "/", nil), rec, &pinned) {
		t.Fatal("cooled account without allow-failover must be answered locally")
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d want 429", rec.Code)
	}
	if pinned.ID != "u-1" {
		t.Fatalf("pinned account mutated to %s", pinned.ID)
	}

	// Cooldown with the header and a healthy alternative: fail over.
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("X-M365-Allow-Failover", "true")
	failed, _ := store.EnsureValid("u-1")
	rec = httptest.NewRecorder()
	if !s.handleCooldownGate(r, rec, &failed) {
		t.Fatal("allow-failover with a healthy alternative must pass the gate")
	}
	if failed.ID == "u-1" {
		t.Fatal("allow-failover must move off the cooled account")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("failover path wrote status %d", rec.Code)
	}

	// Cooldown with the header but the other accounts also cooled: local 429.
	h.MarkFailure("u-2", &UpstreamHTTPError{Status: 429}, 10*time.Minute)
	h.MarkFailure("u-3", &UpstreamHTTPError{Status: 429}, 10*time.Minute)
	last, _ := store.EnsureValid("u-1")
	r2 := httptest.NewRequest(http.MethodPost, "/", nil)
	r2.Header.Set("X-M365-Allow-Failover", "true")
	rec = httptest.NewRecorder()
	if s.handleCooldownGate(r2, rec, &last) {
		t.Fatal("allow-failover without any healthy account must fall back to local 429")
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d want 429", rec.Code)
	}
}

func TestExpiredLimitedCooldowns(t *testing.T) {
	h := newAccountHealth()
	h.mu.Lock()
	h.limited["expired"] = true
	h.cooldown["expired"] = time.Now().Add(-time.Second)
	h.limited["cooling"] = true
	h.cooldown["cooling"] = time.Now().Add(time.Hour)
	h.limited["clean"] = true
	h.mu.Unlock()
	got := h.ExpiredLimitedCooldowns()
	if _, ok := got["expired"]; !ok {
		t.Fatal("expired limited account must be a probe candidate")
	}
	if _, ok := got["cooling"]; ok {
		t.Fatal("cooling account must not be a probe candidate")
	}
	if _, ok := got["clean"]; ok {
		t.Fatal("limited account without a cooldown entry is not expired")
	}
	// The snapshot must not clean up state (that is the next real reader's
	// job). RateLimited() would run its own inline cleanup, so inspect the
	// maps directly under the lock.
	h.mu.Lock()
	stillLimited := h.limited["expired"]
	stillCooled := h.cooldown["expired"]
	h.mu.Unlock()
	if !stillLimited || stillCooled.IsZero() {
		t.Fatal("snapshot must not clean up the expired limited state")
	}
}

func TestThrottleMetricsSnapshot(t *testing.T) {
	m := newThrottleMetrics(time.Hour, 10)
	m.record(ThrottleUpstream429, "acc-a", "throttled")
	m.record(ThrottleUpstream429, "acc-a", "throttled again")
	m.record(ThrottleLocalShortCircuit, "acc-a", "cooldown_remaining=30s")
	m.record(ThrottleDebounceReplay, "", "status=429")
	m.record(ThrottleFailover, "acc-b", "nextHealthyAccountExcluding")
	m.record(ThrottleProbeRecovered, "acc-b", "")

	snap := m.snapshot()
	totals := snap["totals"].(accountThrottleCounters)
	if totals.Upstream429 != 2 || totals.LocalShortCircuit != 1 || totals.DebounceReplay != 1 || totals.Failovers != 1 || totals.ProbeRecovered != 1 {
		t.Fatalf("totals=%+v", totals)
	}
	rows := snap["accounts"].([]throttleAccountRow)
	if len(rows) != 2 {
		t.Fatalf("accounts rows=%d want 2", len(rows))
	}
	if rows[0].AccountID != "acc-a" || rows[0].Upstream429 != 2 {
		t.Fatalf("rows must sort by upstream429 desc, got %+v", rows[0])
	}
	events := snap["events"].([]throttleEvent)
	if len(events) != 6 || events[0].Kind != ThrottleProbeRecovered {
		t.Fatalf("events must be newest-first, got %d entries first=%v", len(events), events[0])
	}

	// Ring cap: recording past maxEvents keeps the newest half, not zero.
	for i := 0; i < 30; i++ {
		m.record(ThrottleUpstream429, "acc-c", "burst")
	}
	snap = m.snapshot()
	if got := len(snap["events"].([]throttleEvent)); got == 0 || got > 10 {
		t.Fatalf("event ring must stay bounded (got %d)", got)
	}

	m.reset()
	snap = m.snapshot()
	if snap["totals"].(accountThrottleCounters).Upstream429 != 0 || len(snap["events"].([]throttleEvent)) != 0 {
		t.Fatal("reset must clear counters and events")
	}
}

// Deixis is a property of the CURRENT user turn. Scanning every user message
// in the window made the upgrade fire on 72% of real agent requests because
// words like "continue" appear in older instructions, erasing the slimming
// benefit.
func TestRouterWindowNeedsFullHistoryOnlyLatestUserMessage(t *testing.T) {
	window := []oaiMsg{
		{Role: "user", Content: "继续刚才的任务，先读取配置文件"},
		{Role: "assistant", Content: "done"},
		{Role: "user", Content: "现在检查输出目录里的文件列表"},
	}
	if routerWindowNeedsFullHistory(window) {
		t.Fatal("hint only in an older message must not upgrade")
	}
	window = append(window, oaiMsg{Role: "user", Content: "继续"})
	if !routerWindowNeedsFullHistory(window) {
		t.Fatal("hint in the latest user message must upgrade")
	}
}

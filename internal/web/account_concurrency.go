package web

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"

	"m365-copilot2api/internal/chathub"
)

const defaultAccountConcurrency = 8

// accountConcurrencyEnvVars lists the accepted env vars in precedence order.
// M365_ACCOUNT_DEFAULT_CONCURRENCY is the documented name;
// M365_ACCOUNT_CONCURRENCY_LIMIT is a legacy alias kept for compatibility.
var accountConcurrencyEnvVars = []string{"M365_ACCOUNT_DEFAULT_CONCURRENCY", "M365_ACCOUNT_CONCURRENCY_LIMIT"}

type accountConcurrency struct {
	mu       sync.Mutex
	limit    int
	inflight map[string]int
	changed  chan struct{}

	// envLocked marks an explicit env override, which always wins over values
	// persisted from the web console. limitFn is bound once during server
	// construction, before any request is served.
	envLocked bool
	limitFn   func() int
}

func accountConcurrencyFromEnv() (int, bool) {
	for _, name := range accountConcurrencyEnvVars {
		raw := strings.TrimSpace(os.Getenv(name))
		if raw == "" {
			continue
		}
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			return parsed, true
		}
	}
	return defaultAccountConcurrency, false
}

func newAccountConcurrency() *accountConcurrency {
	limit, locked := accountConcurrencyFromEnv()
	return &accountConcurrency{limit: limit, envLocked: locked, inflight: map[string]int{}, changed: make(chan struct{})}
}

// bindLimitProvider wires runtime settings into the gate so that changing the
// account concurrency in the web console takes effect without a restart. It must
// be called during construction, before the server starts serving traffic.
func (c *accountConcurrency) bindLimitProvider(fn func() int) {
	if c == nil || c.envLocked {
		return
	}
	c.limitFn = fn
}

// currentLimit resolves the effective per-account ceiling. It must be called
// without holding c.mu.
func (c *accountConcurrency) currentLimit() int {
	if c == nil {
		return defaultAccountConcurrency
	}
	if c.limitFn != nil {
		if n := c.limitFn(); n > 0 {
			return n
		}
	}
	if c.limit > 0 {
		return c.limit
	}
	return defaultAccountConcurrency
}

func (c *accountConcurrency) Available(accountID string) bool {
	if c == nil || accountID == "" {
		return true
	}
	limit := c.currentLimit()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inflight[accountID] < limit
}

func (c *accountConcurrency) Acquire(ctx context.Context, accountID string) (func(), error) {
	if c == nil || accountID == "" {
		return func() {}, nil
	}
	for {
		limit := c.currentLimit()
		c.mu.Lock()
		if c.inflight[accountID] < limit {
			c.inflight[accountID]++
			c.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					c.mu.Lock()
					if c.inflight[accountID] <= 1 {
						delete(c.inflight, accountID)
					} else {
						c.inflight[accountID]--
					}
					close(c.changed)
					c.changed = make(chan struct{})
					c.mu.Unlock()
				})
			}, nil
		}
		changed := c.changed
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func (c *accountConcurrency) Snapshot() map[string]any {
	if c == nil {
		return map[string]any{"limit": defaultAccountConcurrency, "inflight": map[string]int{}}
	}
	limit := c.currentLimit()
	c.mu.Lock()
	defer c.mu.Unlock()
	inflight := make(map[string]int, len(c.inflight))
	for accountID, count := range c.inflight {
		inflight[accountID] = count
	}
	return map[string]any{"limit": limit, "inflight": inflight}
}

func (c *accountConcurrency) Inflight(accountID string) int {
	if c == nil || accountID == "" {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inflight[accountID]
}

func (s *Server) accountAvailable(accountID string) bool {
	if s.tokens != nil && !s.tokens.ScheduleEnabled(accountID) {
		return false
	}
	return s.accountPool.Available(accountID) && s.accountConcurrency.Available(accountID)
}

func (s *Server) accountClient(accountID string) *chathub.Client {
	if acc, ok := s.tokens.Get(accountID); ok && acc.BoundProxy != "" {
		return s.clientForProxy(acc.BoundProxy)
	}
	return s.chat
}

func (s *Server) recordAllowanceConsumption(accountID string, request chathub.Request) {
	if s == nil || s.accountPool == nil {
		return
	}
	capability := request.Capability
	if capability == "" {
		capability = "LLMOnly"
	}
	s.accountPool.RecordAllowanceConsumption(accountID, capability)
}

func (s *Server) chatWithAccount(ctx context.Context, accountID string, account chathub.Account, request chathub.Request) (chathub.Result, error) {
	release, err := s.accountConcurrency.Acquire(ctx, accountID)
	if err != nil {
		return chathub.Result{}, err
	}
	defer release()
	if s.accountPool != nil {
		s.accountPool.MarkCall(accountID)
	}
	result, err := s.accountClient(accountID).Chat(ctx, account, request)
	s.recordAccountResultForCapability(accountID, result, err, request.Capability)
	if err == nil {
		s.recordAllowanceConsumption(accountID, request)
	}
	return result, err
}

func (s *Server) chatWithAccountEvents(ctx context.Context, accountID string, account chathub.Account, request chathub.Request, onEvent func(chathub.StreamEvent) error) (chathub.Result, error) {
	release, err := s.accountConcurrency.Acquire(ctx, accountID)
	if err != nil {
		return chathub.Result{}, err
	}
	defer release()
	if s.accountPool != nil {
		s.accountPool.MarkCall(accountID)
	}
	result, err := s.accountClient(accountID).ChatWithEvents(ctx, account, request, onEvent)
	s.recordAccountResultForCapability(accountID, result, err, request.Capability)
	if err == nil {
		s.recordAllowanceConsumption(accountID, request)
	}
	return result, err
}

func (s *Server) chatWithAccountReasoning(ctx context.Context, accountID string, account chathub.Account, request chathub.Request, onDelta, onReasoning func(string) error) (chathub.Result, error) {
	release, err := s.accountConcurrency.Acquire(ctx, accountID)
	if err != nil {
		return chathub.Result{}, err
	}
	defer release()
	if s.accountPool != nil {
		s.accountPool.MarkCall(accountID)
	}
	result, err := s.accountClient(accountID).ChatWithReasoning(ctx, account, request, onDelta, onReasoning)
	s.recordAccountResultForCapability(accountID, result, err, request.Capability)
	if err == nil {
		s.recordAllowanceConsumption(accountID, request)
	}
	return result, err
}

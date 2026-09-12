package chathub

import (
	"net/http"
	"testing"

	"m365-copilot2api/internal/outbound"
)

// A proxy-bound client pins HTTPClient on purpose, and that pin must win: its
// whole reason to exist is that every call goes through one specific proxy.
func TestHTTPClientForRequestHonoursPinnedClient(t *testing.T) {
	pinned := &http.Client{}
	c := &Client{HTTPClient: pinned}
	if got := c.HTTPClientForRequest(); got != pinned {
		t.Fatal("a pinned HTTPClient must be returned unchanged")
	}
}

// The default client must NOT pin a client at construction. Pinning froze the
// proxy choice for the process lifetime, so an entry the pool had already marked
// dead (from WebSocket or other HTTP traffic) kept receiving every upload.
func TestNewClientDoesNotPinHTTPClient(t *testing.T) {
	c := NewClient()
	if c.HTTPClient != nil {
		t.Fatal("NewClient must leave HTTPClient nil so each request re-selects")
	}
	if c.HTTPClientForRequest() == nil {
		t.Fatal("the per-request accessor must still resolve a usable client")
	}
}

// With a pool configured, successive calls must consult it rather than reuse one
// frozen selection. Each pick builds a fresh client wrapping a poolRoundTripper,
// so distinct pointers per call is the observable signal that selection is
// actually happening per request.
func TestHTTPClientForRequestReselectsPerCallWithPool(t *testing.T) {
	if err := outbound.ConfigurePool([]string{"http://127.0.0.1:38121", "http://127.0.0.1:38122"}); err != nil {
		t.Fatalf("ConfigurePool: %v", err)
	}
	t.Cleanup(func() { _ = outbound.Configure("") })

	c := NewClient()
	first := c.HTTPClientForRequest()
	second := c.HTTPClientForRequest()
	if first == nil || second == nil {
		t.Fatal("pool-backed selection returned no client")
	}
	if first == second {
		t.Fatal("selection is still frozen: both calls returned the same client")
	}
}

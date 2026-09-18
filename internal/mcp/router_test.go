package mcp

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The MCP endpoints get their authentication from the validator injected at
// NewRouter. A nil validator must fail closed — the historical package-level
// APIKeyValidator defaulted to open when unset, which silently exposed the
// endpoints whenever the injector was forgotten.
func TestNewRouterFailClosedWithoutValidator(t *testing.T) {
	h := NewRouter(nil)
	for _, path := range []string{"/v1/mcp/tools", "/v1/mcp/sse", "/v1/mcp/message"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if path == "/v1/mcp/message" {
			req = httptest.NewRequest(http.MethodPost, path, nil)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s with nil validator: got status %d, want %d (fail closed)", path, rec.Code, http.StatusServiceUnavailable)
		}
	}
}

func TestNewRouterRejectsInvalidKey(t *testing.T) {
	h := NewRouter(func(r *http.Request) bool { return false })
	req := httptest.NewRequest(http.MethodGet, "/v1/mcp/tools", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("rejected key: got status %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestNewRouterAllowsValidKey(t *testing.T) {
	h := NewRouter(func(r *http.Request) bool { return true })
	// The tools endpoint needs the registry lock only; other endpoints block on
	// session/SSE, so /v1/mcp/tools is the non-streaming path to assert.
	req := httptest.NewRequest(http.MethodGet, "/v1/mcp/tools", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("valid key: got status %d, want %d", rec.Code, http.StatusOK)
	}
}

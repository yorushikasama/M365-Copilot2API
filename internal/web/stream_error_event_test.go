package web

import (
	"fmt"
	"strings"
	"testing"

	"m365-copilot2api/internal/chathub"
)

func errMap(t *testing.T, ev map[string]any) map[string]any {
	t.Helper()
	em, ok := ev["error"].(map[string]any)
	if !ok {
		t.Fatalf("event lacks error object: %v", ev)
	}
	return em
}

func TestStreamErrorEventCarriesRealCategory(t *testing.T) {
	// The exact shape seen in production logs when a large tool-heavy stream is
	// cut off: the upstream WebSocket went silent and the read timed out.
	err := fmt.Errorf("ws dial: ws read before completion: read tcp 10.0.0.3:12345->40.104.121.146:443: i/o timeout")
	ev := streamErrorEvent("req-abc", err)
	em := errMap(t, ev)
	if em["code"] != "upstream_timeout" {
		t.Fatalf("code = %v, want upstream_timeout", em["code"])
	}
	if em["category"] != "WS_READ_TIMEOUT" {
		t.Fatalf("category = %v, want WS_READ_TIMEOUT", em["category"])
	}
	if em["request_id"] != "req-abc" {
		t.Fatalf("request_id = %v, want req-abc", em["request_id"])
	}
	msg, _ := em["message"].(string)
	if msg == "" || !strings.Contains(msg, "took too long") {
		t.Fatalf("message does not explain the timeout: %q", msg)
	}
	if strings.Contains(msg, "ws dial") || strings.Contains(msg, "tcp") {
		t.Fatalf("message leaks transport internals: %q", msg)
	}
}

func TestStreamErrorEventRateLimitStaysRateLimit(t *testing.T) {
	for _, err := range []error{chathub.ErrMeteringThrottled, chathub.ErrRateLimitNotice, fmt.Errorf("%w: upstream http 429", &UpstreamHTTPError{Status: 429})} {
		ev := streamErrorEvent("req-rl", err)
		em := errMap(t, ev)
		if em["code"] != "rate_limit_error" {
			t.Fatalf("%v: code = %v, want rate_limit_error", err, em["code"])
		}
	}
}

func TestStreamErrorEventOffensiveContentStaysContentBlock(t *testing.T) {
	ev := streamErrorEvent("req-cp", chathub.ErrOffensiveContent)
	em := errMap(t, ev)
	if em["code"] != contentPolicyErrorCode {
		t.Fatalf("code = %v, want %v", em["code"], contentPolicyErrorCode)
	}
	if em["category"] != string(CategoryUpstreamStructured) {
		t.Fatalf("category = %v, want %v", em["category"], CategoryUpstreamStructured)
	}
	msg, _ := em["message"].(string)
	if !strings.Contains(msg, "content policy") {
		t.Fatalf("message does not mention content policy: %q", msg)
	}
}

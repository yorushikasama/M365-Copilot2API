package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"m365-copilot2api/internal/chathub"
)

// A caller that hangs up is not an upstream fault. Claude CLI abandons
// /v1/messages at ~125s while the server allows 300, which used to produce a
// steady stream of misleading 502s.
func TestClientCancelIsNotBadGateway(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"bare context.Canceled", context.Canceled},
		{"wrapped context.Canceled", fmt.Errorf("chat failed: %w", context.Canceled)},
		{"dial error kind", &chathub.DialError{Status: 0, Kind: "CLIENT_CANCELED"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := upstreamStatus(tc.err); got != statusClientClosedRequest {
				t.Fatalf("upstreamStatus = %d, want %d", got, statusClientClosedRequest)
			}
			if !IsClientCanceled(tc.err) {
				t.Fatal("IsClientCanceled = false, want true")
			}
		})
	}

	// A genuine upstream failure must still be a 502.
	if got := upstreamStatus(fmt.Errorf("ws read: connection reset")); got != http.StatusBadGateway {
		t.Fatalf("real upstream failure mapped to %d, want 502", got)
	}
	if IsClientCanceled(nil) {
		t.Fatal("IsClientCanceled(nil) = true, want false")
	}
}

func TestWriteUpstreamErrorClientCancelBody(t *testing.T) {
	rec := httptest.NewRecorder()
	writeUpstreamErrorWithAccount(rec, context.Canceled, "acct-1")

	if rec.Code != statusClientClosedRequest {
		t.Fatalf("status = %d, want %d", rec.Code, statusClientClosedRequest)
	}
	if got := rec.Header().Get("X-M365-Proxy-Error"); got != string(CategoryClientCanceled) {
		t.Fatalf("X-M365-Proxy-Error = %q, want %q", got, CategoryClientCanceled)
	}
	// A cancel is not a rate limit, so no back-off hint may be attached.
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After = %q, want empty", got)
	}
	var payload struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Error.Type != "client_closed_request" {
		t.Fatalf("error.type = %q, want client_closed_request", payload.Error.Type)
	}
}

// writeUpstreamError used to be a line-for-line copy of the account-aware form,
// so every error-mapping change had to be made twice. It must now share it.
func TestWriteUpstreamErrorMatchesAccountVariant(t *testing.T) {
	for _, err := range []error{context.Canceled, chathub.ErrImageLimit, fmt.Errorf("boom")} {
		plain := httptest.NewRecorder()
		writeUpstreamError(plain, err)
		withAcct := httptest.NewRecorder()
		writeUpstreamErrorWithAccount(withAcct, err, "")

		if plain.Code != withAcct.Code {
			t.Fatalf("err=%v: status %d != %d", err, plain.Code, withAcct.Code)
		}
		if plain.Body.String() != withAcct.Body.String() {
			t.Fatalf("err=%v: body %q != %q", err, plain.Body.String(), withAcct.Body.String())
		}
	}
}

// The Anthropic bridge buffers the whole turn before replaying it, so without an
// early flush a streaming caller sees nothing until the upstream finishes and its
// idle timer fires first.
func TestAnthropicStreamKeepaliveFlushesEarlyAndFillsSilence(t *testing.T) {
	t.Setenv("M365_SSE_KEEPALIVE_SECONDS", "1")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	ka := startAnthropicStreamKeepalive(rec, req)
	if ka == nil {
		t.Fatal("keepalive not started despite a flushable writer")
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}

	// Stand in for a slow upstream turn, then stop before reading the body:
	// stop() joins the goroutine, so there is no concurrent writer left.
	time.Sleep(2500 * time.Millisecond)
	ka.stop()

	body := rec.Body.String()
	if !strings.Contains(body, ": connected") {
		t.Fatalf("no preamble flushed; body = %q", body)
	}
	if n := strings.Count(body, ": keepalive"); n < 2 {
		t.Fatalf("got %d keepalive frames in 2.5s at a 1s cadence, want >= 2; body = %q", n, body)
	}
	// Comments only: an Anthropic stream must still begin with message_start.
	if strings.Contains(body, "event:") {
		t.Fatalf("keepalive emitted a protocol event, want SSE comments only; body = %q", body)
	}

	// stop() must be idempotent; handlers call it on every path.
	ka.stop()
}

// Once the stream is committed as 200 the status line is gone, so a late failure
// has to be reported in-band instead of being silently swallowed.
func TestWriteAnthropicFailureAfterCommitUsesSSEEvent(t *testing.T) {
	committed := httptest.NewRecorder()
	writeAnthropicFailure(committed, true, http.StatusBadGateway, "api_error", "upstream protocol error")
	body := committed.Body.String()
	if !strings.Contains(body, "event: error") {
		t.Fatalf("committed stream did not emit an SSE error event; body = %q", body)
	}
	if committed.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: the header was already on the wire", committed.Code)
	}

	// Before commit the normal status line is still available.
	fresh := httptest.NewRecorder()
	writeAnthropicFailure(fresh, false, http.StatusBadGateway, "api_error", "upstream protocol error")
	if fresh.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", fresh.Code)
	}
	if strings.Contains(fresh.Body.String(), "event: error") {
		t.Fatalf("uncommitted response should use a JSON error, got %q", fresh.Body.String())
	}
}

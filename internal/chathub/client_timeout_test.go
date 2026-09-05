package chathub

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestNewClientTimeoutDefaults(t *testing.T) {
	c := NewClient()
	if c.ReadFrameTimeout != defaultFrameReadTimeout {
		t.Fatalf("ReadFrameTimeout = %v, want %v", c.ReadFrameTimeout, defaultFrameReadTimeout)
	}
	if c.ResponseDeadline != defaultResponseDeadline {
		t.Fatalf("ResponseDeadline = %v, want %v", c.ResponseDeadline, defaultResponseDeadline)
	}
	if c.FirstTokenGrace != defaultFirstTokenGrace {
		t.Fatalf("FirstTokenGrace = %v, want %v", c.FirstTokenGrace, defaultFirstTokenGrace)
	}
	t.Setenv("M365_CHATHUB_FIRST_TOKEN_GRACE_SECONDS", "25")
	if got := NewClient().FirstTokenGrace; got != 25*time.Second {
		t.Fatalf("FirstTokenGrace from env = %v, want 25s", got)
	}
}

func TestEnvSecondsDuration(t *testing.T) {
	if got := envSecondsDuration("M365_CHATHUB_READ_TIMEOUT_SECONDS", defaultFrameReadTimeout); got != defaultFrameReadTimeout {
		t.Fatalf("unset env must fall back to default, got %v", got)
	}
	t.Setenv("M365_CHATHUB_READ_TIMEOUT_SECONDS", "240")
	if got := envSecondsDuration("M365_CHATHUB_READ_TIMEOUT_SECONDS", defaultFrameReadTimeout); got != 240*time.Second {
		t.Fatalf("numeric env must be honored, got %v", got)
	}
	t.Setenv("M365_CHATHUB_READ_TIMEOUT_SECONDS", "bogus")
	if got := envSecondsDuration("M365_CHATHUB_READ_TIMEOUT_SECONDS", defaultFrameReadTimeout); got != defaultFrameReadTimeout {
		t.Fatalf("malformed env must fall back to default, got %v", got)
	}
	t.Setenv("M365_CHATHUB_READ_TIMEOUT_SECONDS", "-10")
	if got := envSecondsDuration("M365_CHATHUB_READ_TIMEOUT_SECONDS", defaultFrameReadTimeout); got != defaultFrameReadTimeout {
		t.Fatalf("non-positive env must fall back to default, got %v", got)
	}
}

func TestClientTimeoutAccessors(t *testing.T) {
	var zero *Client
	if got := zero.readFrameTimeout(); got != defaultFrameReadTimeout {
		t.Fatalf("nil client readFrameTimeout = %v, want %v", got, defaultFrameReadTimeout)
	}
	c := &Client{ReadFrameTimeout: 5 * time.Minute, ResponseDeadline: 10 * time.Minute}
	if got := c.readFrameTimeout(); got != 5*time.Minute {
		t.Fatalf("readFrameTimeout = %v, want 5m", got)
	}
	if got := c.responseDeadline(); got != 10*time.Minute {
		t.Fatalf("responseDeadline = %v, want 10m", got)
	}
	if got := zero.firstTokenGrace(); got != defaultFirstTokenGrace {
		t.Fatalf("nil client firstTokenGrace = %v, want %v", got, defaultFirstTokenGrace)
	}
}

// The pre-token budget must exceed the inter-frame timeout, because the silence
// before the first token also covers the upstream reading the whole prompt.
func TestFirstTokenTimeoutScalesWithPayload(t *testing.T) {
	c := &Client{ReadFrameTimeout: 150 * time.Second, FirstTokenGrace: 60 * time.Second, ResponseDeadline: 10 * time.Minute}
	cases := []struct {
		name    string
		payload int
		want    time.Duration
	}{
		{"empty", 0, 210 * time.Second},
		{"at grace unit", payloadGraceUnit, 210 * time.Second},
		{"two grace units", 2 * payloadGraceUnit, 230 * time.Second},
		{"huge payload hits grace cap", 1 << 20, 210*time.Second + maxPayloadGrace},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := c.firstTokenTimeout(tc.payload)
			if got != tc.want {
				t.Fatalf("firstTokenTimeout(%d) = %v, want %v", tc.payload, got, tc.want)
			}
			if got < c.readFrameTimeout() {
				t.Fatalf("pre-token budget %v must not be tighter than the inter-frame timeout %v", got, c.readFrameTimeout())
			}
		})
	}
}

// Waiting past the response deadline would only hold an account slot open with
// no chance of a result, so the deadline wins over any payload-derived grace.
func TestFirstTokenTimeoutCappedByResponseDeadline(t *testing.T) {
	c := &Client{ReadFrameTimeout: 150 * time.Second, FirstTokenGrace: 60 * time.Second, ResponseDeadline: 90 * time.Second}
	if got := c.firstTokenTimeout(1 << 20); got != 90*time.Second {
		t.Fatalf("firstTokenTimeout = %v, want it capped at the 90s response deadline", got)
	}
}

func TestIsSafeToRetry(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain error carries no phase information", errors.New("boom"), false},
		{"timed out while still silent", &DialError{Status: 0, Kind: "WS_READ_TIMEOUT"}, true},
		{"timed out mid-answer", &DialError{Status: 0, Kind: "WS_READ_TIMEOUT", Streamed: true}, false},
		{"pre-send failures left nothing on the wire", &DialError{Status: 0, Kind: "WS_HANDSHAKE"}, true},
		{"wrapped still resolves", fmt.Errorf("chat: %w", &DialError{Status: 0, Kind: "TCP"}), true},
		{"wrapped streamed still refuses", fmt.Errorf("chat: %w", &DialError{Status: 0, Kind: "TCP", Streamed: true}), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsSafeToRetry(tc.err); got != tc.want {
				t.Fatalf("IsSafeToRetry(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

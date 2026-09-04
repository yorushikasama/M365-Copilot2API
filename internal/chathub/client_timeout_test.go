package chathub

import (
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
}

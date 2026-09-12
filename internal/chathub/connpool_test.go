package chathub

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// newParkTestPool spins up a WebSocket echo endpoint and a pool wired to it.
// server pushes whatever the returned send function is given.
func newParkTestPool(t *testing.T) (*ConnPool, func(string), func()) {
	t.Helper()
	upgrader := websocket.Upgrader{}
	outbound := make(chan string, 16)
	ready := make(chan struct{})
	var once bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		// Answer the SignalR handshake so Warm() accepts the connection.
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{}`+rs))
		if !once {
			once = true
			close(ready)
		}
		for msg := range outbound {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
				return
			}
		}
	}))

	pool := NewConnPool(&websocket.Dialer{}, http.Header{})
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	pool.Warm(context.Background(), Account{OID: "oid-1", TID: "tid-1"}, wsURL, DialOptions{})
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("warm connection never completed the handshake")
	}

	cleanup := func() {
		close(outbound)
		pool.Close()
		srv.Close()
	}
	return pool, func(msg string) { outbound <- msg }, cleanup
}

// A lease whose consumer stops reading must be terminated explicitly. The pump
// used to just return, leaving the consumer blocked on frames forever.
func TestParkedConnectionSignalsStalledConsumer(t *testing.T) {
	prev := parkForwardTTL
	parkForwardTTL = 50 * time.Millisecond
	defer func() { parkForwardTTL = prev }()

	pool, send, cleanup := newParkTestPool(t)
	defer cleanup()

	_, _, frames, errs, reused, err := pool.Take(context.Background(), "oid-1", "tid-1", "", DialOptions{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reused {
		t.Fatal("expected the warmed connection to be reused")
	}

	// Overrun the 64-slot buffer without reading, so the pump blocks on send.
	for i := 0; i < 80; i++ {
		send(`{"type":1,"target":"update"}`)
	}

	select {
	case err := <-errs:
		if !errors.Is(err, errParkConsumerStalled) {
			t.Fatalf("errs = %v, want errParkConsumerStalled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stalled lease never reported an error; consumers would block forever")
	}

	// frames must be closed so a blocked receive unblocks instead of hanging.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-frames:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("frames was never closed after the lease died")
		}
	}
}

// park is the pool's only entry point and must hand the connection straight to
// the pump. A connection a caller has read from itself can never be parked: the
// pump would become a SECOND reader sharing gorilla's frame parser, and the
// corruption only shows up on the next request that takes the connection.
func TestParkPublishesConnectionForReuse(t *testing.T) {
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	pool := NewConnPool(&websocket.Dialer{}, http.Header{})
	defer pool.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, _, _, reused, err := pool.Take(context.Background(), "oid-2", "tid-2", wsURL, DialOptions{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if reused {
		t.Fatal("empty pool must produce a fresh dial")
	}
	if got := pool.Stats()["pooled_connections"]; got != 0 {
		t.Fatalf("pooled_connections = %v, want 0 before park", got)
	}

	pool.park(pool.key("oid-2", "tid-2", DialOptions{}.Signature()), conn)

	if got := pool.Stats()["pooled_connections"]; got != 1 {
		t.Fatalf("pooled_connections = %v, want 1 after park", got)
	}
}

// A leased connection must be taken once and then dropped, never re-parked: it
// already owns the pump as its permanent reader for life.
func TestLeasedConnectionIsDroppedNotReparked(t *testing.T) {
	pool, _, cleanup := newParkTestPool(t)
	defer cleanup()

	conn, _, _, _, reused, err := pool.Take(context.Background(), "oid-1", "tid-1", "", DialOptions{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reused {
		t.Fatal("expected a pooled hit")
	}

	pool.Discard("oid-1", "tid-1", conn)

	if got := pool.Stats()["pooled_connections"]; got != 0 {
		t.Fatalf("pooled_connections = %v, want 0: a leased connection must not be re-parked", got)
	}
	if got := pool.Stats()["leased_connections"]; got != 0 {
		t.Fatalf("leased_connections = %v, want 0 after Discard", got)
	}
}

// BypassPool forces a fresh dial: a request that just hit a transport-level
// failure must not be handed the same parked connection (or its siblings) again.
func TestTakeBypassPoolSkipsParkedConnection(t *testing.T) {
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		// Answer the SignalR handshake so Warm() accepts the connection.
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{}`+rs))
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	pool := NewConnPool(&websocket.Dialer{}, http.Header{})
	defer pool.Close()
	pool.Warm(context.Background(), Account{OID: "oid-1", TID: "tid-1"}, wsURL, DialOptions{})
	for i := 0; i < 200; i++ {
		if pool.Stats()["pooled_connections"] == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := pool.Stats()["pooled_connections"]; got != 1 {
		t.Fatalf("sanity: expected 1 parked connection, got %v", got)
	}

	conn, _, _, _, reused, err := pool.Take(context.Background(), "oid-1", "tid-1", wsURL, DialOptions{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if reused {
		t.Fatal("bypassPool=true must not reuse a parked connection")
	}
	if conn == nil {
		t.Fatal("bypassPool=true must still return a usable connection")
	}
	if got := pool.Stats()["pooled_connections"]; got != 1 {
		t.Fatalf("pooled_connections = %v, want 1: bypass must leave the parked connection alone", got)
	}
}

// Discard must always drop the lease bookkeeping so the map cannot grow forever.
func TestDiscardClearsLease(t *testing.T) {
	pool, _, cleanup := newParkTestPool(t)
	defer cleanup()

	conn, _, _, _, _, err := pool.Take(context.Background(), "oid-1", "tid-1", "", DialOptions{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := pool.Stats()["leased_connections"]; got != 1 {
		t.Fatalf("leased_connections = %v, want 1 while checked out", got)
	}
	pool.Discard("oid-1", "tid-1", conn)
	if got := pool.Stats()["leased_connections"]; got != 0 {
		t.Fatalf("leased_connections = %v, want 0 after Discard", got)
	}
}

// A parked connection carries dial-time flags that the chat payload cannot
// override: upstream reads memory behaviour from the WebSocket URL, and the
// payload has no equivalent field. The pool therefore may only hand a
// connection to a request whose memory flag matches.
//
// Regression for the 2026-09-11 finding: the key was oid|tid alone, so a request
// that had upstream memory disabled (the default, M365_ENABLE_UPSTREAM_MEMORY
// unset) was routinely served a warmed connection dialed WITH memory enabled.
// Live probe: three "repeat any token you were told" requests and an explicit
// copilot_temp_session control all returned the same stale account memory.
func TestPoolKeySeparatesDialIdentity(t *testing.T) {
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{}`+rs))
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	pool := NewConnPool(&websocket.Dialer{}, http.Header{})
	defer pool.Close()

	memoryOn := DialOptions{Scenario: "OfficeWebIncludedCopilot"}
	memoryOff := DialOptions{Scenario: "OfficeWebIncludedCopilot", DisableMemory: true}

	pool.Warm(context.Background(), Account{OID: "oid-9", TID: "tid-9"}, wsURL, memoryOn)
	for i := 0; i < 200 && pool.Stats()["pooled_connections"] != 1; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if got := pool.Stats()["pooled_connections"]; got != 1 {
		t.Fatalf("sanity: expected 1 parked memory-enabled connection, got %v", got)
	}

	conn, _, _, _, reused, err := pool.Take(context.Background(), "oid-9", "tid-9", wsURL, memoryOff, false)
	if err != nil {
		t.Fatal(err)
	}
	if reused {
		t.Fatal("a memory-disabled request was handed a memory-enabled connection")
	}
	if conn == nil {
		t.Fatal("a mismatched identity must still dial a usable connection")
	}
	pool.Discard("oid-9", "tid-9", conn)

	if _, _, _, _, reusedSame, err := pool.Take(context.Background(), "oid-9", "tid-9", wsURL, memoryOn, false); err != nil {
		t.Fatal(err)
	} else if !reusedSame {
		t.Fatal("an identical dial identity must still hit the pool")
	}

	// Conversation and session ids are deliberately NOT part of the key. They are
	// per-request values, and the gateway ships the caller's full history in the
	// payload, so reusing across conversations stays correct — while keying on
	// them would make every agent turn a fresh key and destroy the hit rate.
	pool.Warm(context.Background(), Account{OID: "oid-9", TID: "tid-9"}, wsURL, memoryOn)
	for i := 0; i < 200 && pool.Stats()["pooled_connections"] != 1; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	otherConv := DialOptions{Scenario: "OfficeWebIncludedCopilot", ConversationID: "conv-other", SessionID: "sess-other"}
	if _, _, _, _, reusedOther, err := pool.Take(context.Background(), "oid-9", "tid-9", wsURL, otherConv, false); err != nil {
		t.Fatal(err)
	} else if !reusedOther {
		t.Fatal("pooling must survive a per-request conversation id change")
	}
}

// The signature must separate fields that contain the separator, otherwise two
// different identities would share a pool bucket.
func TestDialOptionsSignatureDistinguishesFields(t *testing.T) {
	a := DialOptions{LicenseType: "a|b", Scenario: "c"}
	b := DialOptions{LicenseType: "a", Scenario: "b|c"}
	if a.Signature() == b.Signature() {
		t.Fatal("length-prefixed signature aliased across field boundaries")
	}
	if (DialOptions{DisableMemory: true}).Signature() == (DialOptions{}).Signature() {
		t.Fatal("DisableMemory must be part of the signature")
	}
	first := DialOptions{DisableMemory: true}
	second := DialOptions{DisableMemory: true}
	if first.Signature() != second.Signature() {
		t.Fatal("signature must be deterministic")
	}
	// Per-request identity must not fragment the key.
	x := DialOptions{Scenario: "s", ConversationID: "one", SessionID: "one"}
	y := DialOptions{Scenario: "s", ConversationID: "two", SessionID: "two"}
	if x.Signature() != y.Signature() {
		t.Fatal("conversation/session must not fragment the pool key")
	}
}

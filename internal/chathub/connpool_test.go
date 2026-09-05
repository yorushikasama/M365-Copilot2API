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

	pool.Warm(context.Background(), Account{OID: "oid-1", TID: "tid-1"}, wsURL)
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

	_, _, frames, errs, reused, err := pool.Take(context.Background(), "oid-1", "tid-1", "")
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

// Return must park a freshly dialed connection so traffic — not just Warm —
// populates the pool.
func TestReturnParksFreshlyDialedConnection(t *testing.T) {
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
	conn, _, _, _, reused, err := pool.Take(context.Background(), "oid-2", "tid-2", wsURL)
	if err != nil {
		t.Fatal(err)
	}
	if reused {
		t.Fatal("empty pool must produce a fresh dial")
	}
	if got := pool.Stats()["pooled_connections"]; got != 0 {
		t.Fatalf("pooled_connections = %v, want 0 before Return", got)
	}

	pool.Return("oid-2", "tid-2", conn)

	if got := pool.Stats()["pooled_connections"]; got != 1 {
		t.Fatalf("pooled_connections = %v, want 1 after Return", got)
	}
}

// A connection taken from the pool already owns a reader goroutine, so Return
// must close it rather than park it a second time.
func TestReturnDoesNotDoubleParkPooledConnection(t *testing.T) {
	pool, _, cleanup := newParkTestPool(t)
	defer cleanup()

	conn, _, _, _, reused, err := pool.Take(context.Background(), "oid-1", "tid-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if !reused {
		t.Fatal("expected a pooled hit")
	}

	pool.Return("oid-1", "tid-1", conn)

	if got := pool.Stats()["pooled_connections"]; got != 0 {
		t.Fatalf("pooled_connections = %v, want 0: a leased connection must not be re-parked", got)
	}
	if got := pool.Stats()["leased_connections"]; got != 0 {
		t.Fatalf("leased_connections = %v, want 0 after Return", got)
	}
}

// Discard must always drop the lease bookkeeping so the map cannot grow forever.
func TestDiscardClearsLease(t *testing.T) {
	pool, _, cleanup := newParkTestPool(t)
	defer cleanup()

	conn, _, _, _, _, err := pool.Take(context.Background(), "oid-1", "tid-1", "")
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

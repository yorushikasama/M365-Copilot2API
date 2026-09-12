package chathub

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// A displaced pool used to be dropped on the floor: its gcLoop goroutine ran
// forever and its parked sockets stayed open. Close must stop the loop.
func TestCloseStopsGCLoop(t *testing.T) {
	before := runtime.NumGoroutine()
	pools := make([]*ConnPool, 0, 16)
	for i := 0; i < 16; i++ {
		pools = append(pools, NewConnPool(&websocket.Dialer{}, http.Header{}))
	}
	for _, p := range pools {
		p.Close()
	}
	// Goroutine teardown is asynchronous; give the scheduler a bounded window.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("gc goroutines survived Close: before=%d after=%d", before, runtime.NumGoroutine())
}

// Close on an already-closed pool must not panic on the second close(p.stop).
func TestCloseIsIdempotent(t *testing.T) {
	p := NewConnPool(&websocket.Dialer{}, http.Header{})
	p.Close()
	p.Close()
}

// A park landing after Close would add a socket to a pool with no GC loop left
// to reap it, so it must be closed instead of parked.
func TestParkAfterCloseDoesNotRetainConnection(t *testing.T) {
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		// Hold the connection open until the client closes it.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				_ = conn.Close()
				return
			}
		}
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := (&websocket.Dialer{}).Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	p := NewConnPool(&websocket.Dialer{}, http.Header{})
	p.Close()
	p.park(p.key("oid", "tid", DialOptions{}.Signature()), conn)

	p.mu.Lock()
	pooled := len(p.conns)
	p.mu.Unlock()
	if pooled != 0 {
		t.Fatalf("park after Close retained %d pool entries", pooled)
	}
	// The socket must be closed, so a write fails.
	if err := conn.WriteMessage(websocket.TextMessage, []byte("x")); err == nil {
		t.Fatal("connection was left open after park on a closed pool")
	}
}

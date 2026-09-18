package chathub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeChathub serves a scripted ChatHub frame sequence over WebSocket, so a
// test can replay the frame interleavings the real upstream produces. wsBase is
// redirected at it for the duration of the test.
type fakeChathub struct {
	srv      *httptest.Server
	conns    atomic.Int64
	frames   func(conn *websocket.Conn)
	upgrader websocket.Upgrader
}

func newFakeChathub(t *testing.T, frames func(conn *websocket.Conn)) *fakeChathub {
	t.Helper()
	f := &fakeChathub{frames: frames}
	f.upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := f.upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		// The client may open a connection it never streams on; only the first
		// scripted connection is driven, so the script is not run twice.
		if f.conns.Add(1) > 1 {
			time.Sleep(2 * time.Second)
			return
		}
		f.frames(c)
	}))
	t.Cleanup(f.srv.Close)

	prev := wsBase
	wsBase = "ws" + strings.TrimPrefix(f.srv.URL, "http")
	t.Cleanup(func() { wsBase = prev })
	return f
}

// rsSep is the record separator between frames on the ChatHub wire.
const rsSep = "\x1e"

// botMsgID is the message id the real upstream uses in cursor.j.
const botMsgID = "00000000-0000-0000-0000-00000000bot1"

// snapshotFrame is a cumulative text snapshot, matching the shape recorded in
// docs/har-mining/05-streaming-frames.md §3.1: the first text frame of a turn
// carries a cursor pointing at the message text, and messages[].text is the
// full text so far (contentOrigin=DeepLeo), strictly monotonic.
func snapshotFrame(text string) string {
	msg := map[string]any{
		"text":          text,
		"author":        "bot",
		"messageId":     botMsgID,
		"contentOrigin": "DeepLeo",
		"adaptiveCards": []any{map[string]any{
			"type": "AdaptiveCard", "version": "1.0",
			"body": []any{map[string]any{"type": "TextBlock", "text": text, "wrap": true}},
		}},
	}
	arg := map[string]any{
		"cursor":    map[string]any{"j": "$['" + botMsgID + "'].adaptiveCards[0].body[0].text", "p": -1},
		"messages":  []any{msg},
		"nonce":     "nonce",
		"requestId": "req",
	}
	return marshalFrame(map[string]any{"type": 1, "target": "update", "arguments": []any{arg}})
}

// cursorDeltaFrame is a writeAtCursor append fragment. Per
// scripts/chathub_probe.py the field lives on the argument itself, alongside
// messages[], not inside a message.
func cursorDeltaFrame(fragment string) string {
	arg := map[string]any{
		"writeAtCursor": fragment,
		"messages": []any{map[string]any{
			"author":    "bot",
			"messageId": botMsgID,
		}},
		"nonce":     "nonce",
		"requestId": "req",
	}
	return marshalFrame(map[string]any{"type": 1, "target": "update", "arguments": []any{arg}})
}

// resultFrame is the type-2 frame carrying the authoritative final message.
func resultFrame(text string) string {
	return marshalFrame(map[string]any{
		"type": 2,
		"item": map[string]any{"result": map[string]any{"value": "Success", "message": text}},
	})
}

// completionFrame is the type-3 frame that ends the turn: the client runs
// finalizeText and returns on it.
func completionFrame() string {
	return marshalFrame(map[string]any{"type": 3})
}

func marshalFrame(v any) string {
	b, _ := json.Marshal(v)
	return string(b) + rsSep
}

// sendScript sends frames with a pause between each, so the client observes
// them as a real stream rather than one coalesced read.
func sendScript(c *websocket.Conn, gap time.Duration, frames ...string) {
	for _, f := range frames {
		if err := c.WriteMessage(websocket.TextMessage, []byte(f)); err != nil {
			return
		}
		time.Sleep(gap)
	}
}

// waitForClientPayload blocks until the client has written its handshake
// request, so a script does not race ahead of the connection.
func waitForClientPayload(c *websocket.Conn, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_ = c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		if _, _, err := c.ReadMessage(); err == nil {
			return true
		} else if ne, ok := err.(interface{ Timeout() bool }); ok && ne.Timeout() {
			continue
		} else {
			return false
		}
	}
	return false
}

// handshakeAck is the SignalR handshake response. The client reads exactly one
// message for it before entering its frame loop, so a script that omits it has
// its first content frame consumed as the handshake reply and silently lost.
const handshakeAck = "{}\x1e"

// startStream performs the handshake with the client and returns false if the
// client never connected. Callers send their scripted frames afterwards.
func startStream(c *websocket.Conn, timeout time.Duration) bool {
	if !waitForClientPayload(c, timeout) {
		return false
	}
	if err := c.WriteMessage(websocket.TextMessage, []byte(handshakeAck)); err != nil {
		return false
	}
	return true
}

// drainReads absorbs whatever the client writes so its writes do not block.
// A read error means the peer went away, and gorilla panics if a failed
// connection is read again, so every read is guarded and any error ends it.
func drainReads(c *websocket.Conn, stop <-chan struct{}) {
	defer func() { _ = recover() }()
	for {
		select {
		case <-stop:
			return
		default:
		}
		if _, _, err := c.ReadMessage(); err != nil {
			return
		}
	}
}

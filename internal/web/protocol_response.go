package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// defaultSSEKeepaliveInterval matches the cadence already used by the OpenAI
// streaming path in server.go.
const defaultSSEKeepaliveInterval = 15 * time.Second

// sseKeepaliveInterval allows tightening the cadence for clients whose idle
// tolerance is shorter than Claude CLI's ~125s.
func sseKeepaliveInterval() time.Duration {
	if raw := strings.TrimSpace(os.Getenv("M365_SSE_KEEPALIVE_SECONDS")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return defaultSSEKeepaliveInterval
}

// anthropicStreamKeepalive commits an SSE response before the upstream turn is
// known, then keeps it visibly alive until the real frames are ready.
//
// /v1/messages assembles the entire turn through runOpenAIAdapter, which forces
// Stream=false into an in-memory recorder and only replays it as SSE afterwards.
// A streaming caller therefore received zero bytes for the whole upstream
// latency, and Claude CLI reads that silence as a dead connection: it hangs up at
// roughly 125s even though M365_CHAT_TIMEOUT_SECONDS allows 300. Flushing the
// headers immediately and dripping bytes while we wait stops the client's idle
// timer from firing.
//
// The filler is an SSE comment rather than an Anthropic `ping` event on purpose:
// the protocol documents that a stream *begins* with message_start, and comments
// are discarded by every spec-compliant parser, so they cannot disturb a client
// state machine that has not seen message_start yet.
type anthropicStreamKeepalive struct {
	done    chan struct{}
	stopped chan struct{}
}

func startAnthropicStreamKeepalive(w http.ResponseWriter, r *http.Request) *anthropicStreamKeepalive {
	f, ok := w.(http.Flusher)
	if !ok {
		// Without a flusher nothing reaches the wire early anyway, so committing
		// the headers would only cost us the ability to report a real status.
		return nil
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ka := &anthropicStreamKeepalive{done: make(chan struct{}), stopped: make(chan struct{})}
	if err := sseRaw(r.Context(), w, f, ": connected\n\n"); err != nil {
		// The response may already be partially committed, so report it as
		// started: an in-band error beats a superfluous WriteHeader.
		close(ka.done)
		close(ka.stopped)
		return ka
	}
	go func() {
		defer close(ka.stopped)
		t := time.NewTicker(sseKeepaliveInterval())
		defer t.Stop()
		for {
			select {
			case <-ka.done:
				return
			case <-r.Context().Done():
				return
			case <-t.C:
				if err := sseRaw(r.Context(), w, f, ": keepalive\n\n"); err != nil {
					return
				}
			}
		}
	}()
	return ka
}

// stop halts the keepalive and waits for its goroutine to exit, handing the
// caller exclusive ownership of the ResponseWriter back before any real frame is
// written. net/http writers are not goroutine-safe, so overlapping the keepalive
// with message_start would risk interleaved half-frames.
func (k *anthropicStreamKeepalive) stop() {
	if k == nil {
		return
	}
	select {
	case <-k.done:
	default:
		close(k.done)
	}
	<-k.stopped
}

// writeAnthropicFailure reports an error for /v1/messages. Once the stream has
// been committed the status line is already on the wire as 200, so the failure
// has to travel in-band as an SSE error event instead.
func writeAnthropicFailure(w http.ResponseWriter, committed bool, status int, typ, msg string) {
	if !committed {
		writeAnthropicError(w, status, typ, msg)
		return
	}
	f, _ := w.(http.Flusher)
	_ = sseWriteFrame(w, f, "error", map[string]any{
		"type":  "error",
		"error": map[string]any{"type": typ, "message": msg},
	})
}

func openAIChoice(v map[string]any) (map[string]any, string) {
	choices, _ := v["choices"].([]any)
	if len(choices) == 0 {
		return nil, ""
	}
	c, _ := choices[0].(map[string]any)
	m, _ := c["message"].(map[string]any)
	finish, _ := c["finish_reason"].(string)
	return m, finish
}

func writeAnthropicResult(w http.ResponseWriter, model string, stream bool, src map[string]any) {
	id := "msg_" + uuid.NewString()
	msg, finish := openAIChoice(src)
	sanitizePublicAssistantMessage(msg, model)
	blocks := []any{}
	stop := "end_turn"
	if reasoning, _ := msg["reasoning_content"].(string); reasoning != "" {
		blocks = append(blocks, map[string]any{"type": "thinking", "thinking": reasoning, "signature": ""})
	}
	if calls, ok := msg["tool_calls"].([]any); ok {
		stop = "tool_use"
		for _, raw := range calls {
			tc, _ := raw.(map[string]any)
			fn, _ := tc["function"].(map[string]any)
			var input any = map[string]any{}
			if a, ok := fn["arguments"].(string); ok {
				_ = json.Unmarshal([]byte(a), &input)
			}
			blocks = append(blocks, map[string]any{"type": "tool_use", "id": tc["id"], "name": fn["name"], "input": input})
		}
	} else {
		switch content := msg["content"].(type) {
		case []any:
			for _, raw := range content {
				part, _ := raw.(map[string]any)
				switch part["type"] {
				case "text":
					if t, _ := part["text"].(string); t != "" {
						blocks = append(blocks, map[string]any{"type": "text", "text": t})
					}
				case "image_url":
					img, _ := part["image_url"].(map[string]any)
					if u, _ := img["url"].(string); u != "" {
						if strings.HasPrefix(u, "data:") {
							parts := strings.SplitN(u, ",", 2)
							meta := parts[0]
							b64 := ""
							if len(parts) == 2 {
								b64 = parts[1]
							}
							media := strings.TrimPrefix(meta, "data:")
							media = strings.SplitN(media, ";", 2)[0]
							blocks = append(blocks, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": media, "data": b64}})
						} else {
							blocks = append(blocks, map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": u}})
						}
					}
				}
			}
		default:
			blocks = append(blocks, map[string]any{"type": "text", "text": fmt.Sprint(content)})
		}
		if len(blocks) == 0 {
			blocks = append(blocks, map[string]any{"type": "text", "text": ""})
		}
	}
	_ = finish
	inputTokens := int64(0)
	outputTokens := int64(0)
	if u, ok := src["usage"].(map[string]any); ok {
		if v, ok := u["prompt_tokens"]; ok {
			if n, ok := v.(int64); ok {
				inputTokens = n
			}
			if n, ok := v.(float64); ok {
				inputTokens = int64(n)
			}
		}
		if v, ok := u["completion_tokens"]; ok {
			if n, ok := v.(int64); ok {
				outputTokens = n
			}
			if n, ok := v.(float64); ok {
				outputTokens = int64(n)
			}
		}
	}
	out := map[string]any{"id": id, "type": "message", "role": "assistant", "model": model, "content": blocks, "stop_reason": stop, "stop_sequence": nil, "usage": map[string]any{"input_tokens": inputTokens, "output_tokens": outputTokens}}
	if !stream {
		jsonOut(w, out)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	f, _ := w.(http.Flusher)
	aborted := false
	emit := func(n string, v any) {
		if aborted {
			return
		}
		if err := sseWriteFrame(w, f, n, v); err != nil {
			aborted = true
		}
	}
	emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": id, "type": "message", "role": "assistant", "model": model, "content": []any{}, "stop_reason": nil, "usage": map[string]any{"input_tokens": inputTokens, "output_tokens": 0}}})
	for i, b := range blocks {
		m, _ := b.(map[string]any)
		startBlock := b
		blockType := ""
		if t, _ := m["type"].(string); t != "" {
			blockType = t
		}
		switch blockType {
		case "tool_use":
			startBlock = map[string]any{"type": "tool_use", "id": m["id"], "name": m["name"], "input": map[string]any{}}
		case "thinking":
			startBlock = map[string]any{"type": "thinking", "thinking": "", "signature": ""}
		case "image":
			startBlock = map[string]any{"type": "image", "source": m["source"]}
		}
		emit("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": startBlock})
		switch blockType {
		case "text":
			emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "text_delta", "text": m["text"]}})
		case "tool_use":
			partial, _ := json.Marshal(m["input"])
			emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(partial)}})
		case "thinking":
			emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "thinking_delta", "thinking": m["thinking"]}})
		}
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
	}
	emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": outputTokens}})
	emit("message_stop", map[string]any{"type": "message_stop"})
}

// sseWriteFrame writes one SSE frame and flushes; a write error (client gone,
// deadline exceeded) aborts the stream instead of leaving the handler blocked.
func sseWriteFrame(w http.ResponseWriter, f http.Flusher, name string, value any) error {
	b, _ := json.Marshal(value)
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b); err != nil {
		return err
	}
	if f != nil {
		f.Flush()
	}
	return nil
}

// sseDataRaw writes a raw "data: ..." frame with the same write deadline.
func sseDataRaw(w http.ResponseWriter, f http.Flusher, data string) error {
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return err
	}
	if f != nil {
		f.Flush()
	}
	return nil
}

// sseSafeRaw writes a pre-formatted frame (e.g. ": connected" or "[DONE]").
func sseSafeRaw(w http.ResponseWriter, f http.Flusher, payload string) error {
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := fmt.Fprint(w, payload); err != nil {
		return err
	}
	if f != nil {
		f.Flush()
	}
	return nil
}

// sseWriter serializes all writes to one streaming response. Keepalive
// goroutines and the main emit loop would otherwise interleave partial
// frames on the shared ResponseWriter (net/http writes are not goroutine-safe).
type sseWriter struct {
	w  http.ResponseWriter
	f  http.Flusher
	mu sync.Mutex
}

func newSSEWriter(w http.ResponseWriter, f http.Flusher) *sseWriter {
	return &sseWriter{w: w, f: f}
}

func (s *sseWriter) raw(payload string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rc := http.NewResponseController(s.w)
	_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := fmt.Fprint(s.w, payload); err != nil {
		return err
	}
	if s.f != nil {
		s.f.Flush()
	}
	return nil
}

func (s *sseWriter) data(data string) error {
	return s.raw("data: " + data + "\n\n")
}

// sseKeepalive is the OpenAI-path twin of anthropicStreamKeepalive: it drips SSE
// comments while the upstream turn is in flight. stop() halts the goroutine and
// waits for it to exit, handing exclusive ownership of the ResponseWriter back
// before any direct (unlocked) tail write — sseRaw, writeSSE and
// writeToolResponse all bypass sseWriter's mutex, so they may only run once the
// keepalive goroutine is provably gone.
type sseKeepalive struct {
	done    chan struct{}
	stopped chan struct{}
}

// startSSEKeepalive begins the comment ticker for a stream whose writer is sw.
// The goroutine ends on stop(), when ctx is done, or when the ticker fires into
// a dead writer.
func startSSEKeepalive(sw *sseWriter, ctx context.Context) *sseKeepalive {
	ka := &sseKeepalive{done: make(chan struct{}), stopped: make(chan struct{})}
	go func() {
		defer close(ka.stopped)
		t := time.NewTicker(sseKeepaliveInterval())
		defer t.Stop()
		for {
			select {
			case <-ka.done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				_ = sw.raw(": keepalive\n\n")
			}
		}
	}()
	return ka
}

// stop is idempotent and safe to defer: the first call closes done and waits for
// the goroutine to finish any in-flight write.
func (k *sseKeepalive) stop() {
	if k == nil {
		return
	}
	select {
	case <-k.done:
	default:
		close(k.done)
	}
	<-k.stopped
}

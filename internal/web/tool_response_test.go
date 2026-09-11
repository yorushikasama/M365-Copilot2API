package web

import (
	"encoding/json"
	"m365-copilot2api/internal/chathub"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// collectStreamedArguments replays an SSE tool-call stream and returns the
// arguments string a client would reassemble from the deltas.
func collectStreamedArguments(t *testing.T, body string) string {
	t.Helper()
	var out strings.Builder
	for _, line := range strings.Split(body, "\n") {
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok || payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Function struct {
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("chunk is not valid JSON: %v (%q)", err, payload)
		}
		for _, choice := range chunk.Choices {
			for _, call := range choice.Delta.ToolCalls {
				out.WriteString(call.Function.Arguments)
			}
		}
	}
	return out.String()
}

func TestWriteToolResponseStreamsMultibyteArgumentsIntact(t *testing.T) {
	// The 512-byte chunker advanced `off` by chunkSize while `end` had moved
	// forward to clear a rune boundary. Every chunk after the first split rune
	// therefore restarted mid-character: json.Marshal replaced the partial
	// sequence with U+FFFD and re-sent the skipped bytes, so CJK arguments
	// arrived corrupted ("检\ufffd查") with a few duplicated bytes. Reassembling
	// the deltas must reproduce the arguments byte for byte.
	args := `{"path":"D:\\repo","note":"` + strings.Repeat("验收闭环", 60) + `"}`
	if utf8.ValidString(args) == false {
		t.Fatalf("test fixture is not valid UTF-8")
	}
	calls := []detectedToolCall{{
		ID:        "call_1",
		Type:      "function",
		Name:      "edit_file",
		Arguments: json.RawMessage(args),
	}}

	rec := httptest.NewRecorder()
	if err := writeToolResponse(rec, "chatcmpl-test", "m365-copilot", true, false, calls, chathub.Result{}); err != nil {
		t.Fatalf("writeToolResponse: %v", err)
	}

	got := collectStreamedArguments(t, rec.Body.String())
	if got != args {
		t.Fatalf("reassembled arguments differ from the source\n got len=%d\nwant len=%d", len(got), len(args))
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Fatalf("reassembled arguments contain a replacement character: %q", got)
	}
}

func TestWriteToolResponseChunksAreIndividuallyValidUTF8(t *testing.T) {
	// Each SSE delta is marshaled on its own, so a chunk that ends or begins
	// mid-rune is corrupted in transit even if the concatenation looks right.
	args := `{"text":"` + strings.Repeat("补齐日期逻辑与表单状态", 80) + `"}`
	calls := []detectedToolCall{{ID: "call_1", Type: "function", Name: "write", Arguments: json.RawMessage(args)}}

	rec := httptest.NewRecorder()
	if err := writeToolResponse(rec, "chatcmpl-test", "m365-copilot", true, false, calls, chathub.Result{}); err != nil {
		t.Fatalf("writeToolResponse: %v", err)
	}

	for _, line := range strings.Split(rec.Body.String(), "\n") {
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok || payload == "[DONE]" {
			continue
		}
		if strings.ContainsRune(payload, utf8.RuneError) {
			t.Fatalf("a streamed chunk carries a replacement character: %q", payload)
		}
	}
}

func TestWriteToolResponseStreamsFinishReasonExactlyOnce(t *testing.T) {
	// isLastArgChunk was derived from off+chunkSize, which no longer matches the
	// rune-aligned advance; the terminal chunk must still be the one that
	// carries finish_reason, and only one chunk may carry it.
	args := `{"text":"` + strings.Repeat("验", 700) + `"}`
	calls := []detectedToolCall{{ID: "call_1", Type: "function", Name: "write", Arguments: json.RawMessage(args)}}

	rec := httptest.NewRecorder()
	if err := writeToolResponse(rec, "chatcmpl-test", "m365-copilot", true, false, calls, chathub.Result{}); err != nil {
		t.Fatalf("writeToolResponse: %v", err)
	}

	body := rec.Body.String()
	if got := strings.Count(body, `"finish_reason":"tool_calls"`); got != 1 {
		t.Fatalf("finish_reason appears %d times, want exactly 1", got)
	}
	// The finish_reason must ride the final arguments delta, not an earlier one.
	lines := []string{}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, `"arguments"`) {
			lines = append(lines, line)
		}
	}
	if len(lines) < 2 {
		t.Fatalf("expected the arguments to be chunked, got %d argument deltas", len(lines))
	}
	if !strings.Contains(lines[len(lines)-1], `"finish_reason":"tool_calls"`) {
		t.Fatalf("last argument delta is missing finish_reason: %q", lines[len(lines)-1])
	}
}

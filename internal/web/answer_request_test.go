package web

import (
	"encoding/json"
	"strings"
	"testing"

	"m365-copilot2api/internal/chathub"
)

func answerRequestTestBody() oaiReq {
	return oaiReq{
		ConversationID: "conversation-1",
		SessionID:      "session-1",
		Tools: []chathub.Tool{{
			Type:     "function",
			Function: json.RawMessage(`{"name":"read_file","parameters":{"type":"object"}}`),
		}},
		ToolChoice: "auto",
	}
}

func TestBuildAnswerRequestRouterOmitsNativePlugins(t *testing.T) {
	req := buildAnswerRequest("[user]\nhello", "magic", answerRequestTestBody(), agentLedger{}, "router", runtimeSettings{}, chathub.FeatureFlags{}, chathubLocale{}, false)
	if len(req.Tools) != 0 || req.ToolChoice != nil {
		t.Fatalf("router answer leaked native tools: tools=%d choice=%#v", len(req.Tools), req.ToolChoice)
	}
	if req.Text != "[user]\nhello" {
		t.Fatalf("empty ledger changed answer prompt: %q", req.Text)
	}
}

func TestBuildAnswerRequestNativeForwardsTools(t *testing.T) {
	req := buildAnswerRequest("[user]\nhello", "magic", answerRequestTestBody(), agentLedger{}, "native", runtimeSettings{}, chathub.FeatureFlags{}, chathubLocale{}, false)
	if len(req.Tools) != 1 || req.ToolChoice != "auto" {
		t.Fatalf("native answer lost tools: tools=%d choice=%#v", len(req.Tools), req.ToolChoice)
	}
}

func TestBuildAnswerRequestAddsCompletedEvidence(t *testing.T) {
	ledger := agentLedger{Completed: []toolEvidence{{ID: "call_1", Name: "read_file", Arguments: `{}`, Result: "ok"}}}
	req := buildAnswerRequest("[user]\nsummarize", "magic", answerRequestTestBody(), ledger, "router", runtimeSettings{}, chathub.FeatureFlags{}, chathubLocale{}, false)
	for _, want := range []string{"EVIDENCE_LEDGER:", "Report only actions supported by completed tool results"} {
		if !strings.Contains(req.Text, want) {
			t.Fatalf("answer prompt missing %q: %s", want, req.Text)
		}
	}
}

// Regression for the duplicate-subtask root cause: the answer request must
// never advertise a self-referential MCP gateway alongside the API plugins.
// The dual channel (API plugin + MCP server pointing back at this process)
// exposed every declared tool twice, so upstream emitted two identical
// subtask calls for one intent.
func TestBuildAnswerRequestSingleToolChannel(t *testing.T) {
	req := buildAnswerRequest("[user]\nhello", "magic", answerRequestTestBody(), agentLedger{}, "router", runtimeSettings{}, chathub.FeatureFlags{}, chathubLocale{}, false)
	if len(req.Tools) != 0 {
		t.Fatalf("router answer must carry no native tools: tools=%d", len(req.Tools))
	}
}

// Regression for the sandbox-hallucination relapse after the MCP gateway was
// removed: the caller DID declare tools, so the answer turn must keep the
// Windows execution guard even though router planning mode withholds the
// native tool schemas (native_tools=0). Without CallerCanExecute the model
// answered "the execution environment has no D:\... and /mnt/data is empty".
func TestBuildAnswerRequestCallerCanExecuteSurvivesRouterMode(t *testing.T) {
	req := buildAnswerRequest("[user]\nhello", "magic", answerRequestTestBody(), agentLedger{}, "router", runtimeSettings{}, chathub.FeatureFlags{}, chathubLocale{}, false)
	if !req.CallerCanExecute {
		t.Fatal("router answer with declared tools must keep CallerCanExecute=true")
	}

	plain := oaiReq{}
	req = buildAnswerRequest("[user]\nhello", "magic", plain, agentLedger{}, "router", runtimeSettings{}, chathub.FeatureFlags{}, chathubLocale{}, false)
	if req.CallerCanExecute {
		t.Fatal("plain chat without declared tools must not claim execution capability")
	}
}

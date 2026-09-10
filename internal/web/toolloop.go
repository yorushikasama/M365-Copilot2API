package web

import (
	"encoding/json"
	"fmt"
	"m365-copilot2api/internal/chathub"
	"strings"

	"github.com/google/uuid"
)

type detectedToolCall struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func toolType(name string, tools []map[string]any) string {
	for _, t := range tools {
		f, _ := t["function"].(map[string]any)
		if n, _ := f["name"].(string); n == name {
			if typ, _ := t["type"].(string); typ != "" {
				return typ
			}
		}
	}
	return "function"
}

func allowedToolNames(tools []map[string]any) map[string]bool {
	out := map[string]bool{}
	for _, t := range tools {
		if f, ok := t["function"].(map[string]any); ok {
			if n, ok := f["name"].(string); ok && n != "" {
				out[n] = true
			}
		}
	}
	return out
}

type rejectedToolCall struct {
	Name   string
	Reason string
}

// validateDetectedToolCalls is the final trust boundary before a model-selected
// call is serialized to the client. ChatHub/native events and model-generated
// routing text are both untrusted: an undeclared name such as "unknown_tool"
// must never escape to Claude Code, Codex, or another local tool runner.
func validateDetectedToolCalls(calls []detectedToolCall, tools []map[string]any, choice any) ([]detectedToolCall, []rejectedToolCall) {
	valid := make([]detectedToolCall, 0, len(calls))
	rejected := make([]rejectedToolCall, 0)
	for _, call := range calls {
		fn := toolFunction(call.Name, tools)
		if fn == nil {
			rejected = append(rejected, rejectedToolCall{Name: call.Name, Reason: "tool was not declared by the client"})
			continue
		}
		if !toolChoiceAllows(choice, call.Name) {
			rejected = append(rejected, rejectedToolCall{Name: call.Name, Reason: "tool_choice does not allow this tool"})
			continue
		}
		args := map[string]any{}
		if len(call.Arguments) == 0 || string(call.Arguments) == "null" {
			call.Arguments = json.RawMessage(`{}`)
		} else if err := json.Unmarshal(call.Arguments, &args); err != nil {
			rejected = append(rejected, rejectedToolCall{Name: call.Name, Reason: "arguments are not a JSON object"})
			continue
		}
		if err := schemaValid(args, fn); err != nil {
			rejected = append(rejected, rejectedToolCall{Name: call.Name, Reason: err.Error()})
			continue
		}
		if call.ID == "" {
			call.ID = callID(call.Name, string(call.Arguments), len(valid))
		}
		if call.Type == "" {
			call.Type = toolType(call.Name, tools)
		}
		valid = append(valid, call)
	}
	return valid, rejected
}

func toolChoiceAllows(choice any, name string) bool {
	if choice == nil {
		return true
	}
	if s, ok := choice.(string); ok {
		return s != "none" && (s != "required" || name != "")
	}
	if m, ok := choice.(map[string]any); ok {
		if f, ok := m["function"].(map[string]any); ok {
			n, _ := f["name"].(string)
			return n == name
		}
		if n, ok := m["name"].(string); ok {
			return n == name
		}
	}
	return true
}

// callID returns a globally unique tool call id. Content hashes previously
// collided when the same tool+arguments was invoked again (duplicate tool call
// id errors from clients), so uniqueness must not depend on call content.
func callID(name, args string, index int) string {
	return "call_" + strings.ReplaceAll(uuid.NewString(), "-", "")
}

func extractToolCalls(text string, tools []map[string]any, choice any) ([]detectedToolCall, bool) {
	allowed := allowedToolNames(tools)
	var out []detectedToolCall
	remaining := text
	for {
		start := strings.Index(remaining, "<m365-tool-call>")
		if start < 0 {
			break
		}
		end := strings.Index(remaining[start:], "</m365-tool-call>")
		if end < 0 {
			break
		}
		end += start
		content := remaining[start+len("<m365-tool-call>") : end]
		remaining = remaining[end+len("</m365-tool-call>"):]
		var raw any
		if json.Unmarshal([]byte(content), &raw) != nil {
			continue
		}
		items := []any{raw}
		if arr, ok := raw.([]any); ok {
			items = arr
		}
		for _, item := range items {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			n, _ := m["name"].(string)
			if !allowed[n] || !toolChoiceAllows(choice, n) {
				continue
			}
			a, _ := json.Marshal(m["arguments"])
			out = append(out, detectedToolCall{ID: callID(n, string(a), len(out)), Type: toolType(n, tools), Name: n, Arguments: a})
		}
	}
	return out, len(out) > 0
}

func validateToolResult(messages []oaiMsg, known map[string]bool) error {
	for _, m := range messages {
		if m.Role == "tool" {
			if m.ToolCallID == "" {
				return fmt.Errorf("tool_call_id required")
			}
			if len(known) > 0 && !known[m.ToolCallID] {
				return fmt.Errorf("unknown tool_call_id: %s", m.ToolCallID)
			}
		}
	}
	return nil
}

var toolRefusalPatterns = []string{
	"tools are not available",
	"tool is not available",
	"not actually registered",
	"not actually available",
	"not available in this session",
	"工具不可用",
	"工具未暴露",
}

// toolDenialPhrases cover the sandbox-hallucination refusal family that
// slipped past toolRefusalPatterns (2026-09-10: gpt-5.6-sol answered an
// execution request with "当前会话没有可调用的 Windows 工作区文件编辑工具" —
// none of the legacy patterns matched, and the reply was 780 chars so the
// short-text cap in isToolRefusal excluded it too).
var toolDenialPhrases = append(toolRefusalPatterns,
	"没有可调用",
	"没有可用的工具",
	"没有工具",
	"无工具可用",
	"无法写入",
	"无法编辑",
	"没有文件编辑",
	"no callable tool",
	"no tools available",
	"no available tools",
	"don't have access to tools",
	"do not have access to tools",
	"don't have any tools",
	"cannot edit files",
	"unable to edit files",
	"cannot write files",
	"no file editing tool",
)

// containsToolDenial reports whether the text denies that callable tools
// exist. Unlike isToolRefusal it has no length cap: refusal prose buried in a
// long status report is exactly the 2026-09-10 failure shape.
func containsToolDenial(text string) bool {
	low := strings.ToLower(text)
	for _, p := range toolDenialPhrases {
		if strings.Contains(low, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

func isToolRefusal(text string) bool {
	if len(text) >= 200 {
		return false
	}
	low := strings.ToLower(text)
	for _, p := range toolRefusalPatterns {
		if strings.Contains(low, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

func isContentPolicyBlock(text string) bool {
	return chathub.IsContentPolicyBlock(text)
}

func isImageLimitNotice(text string) bool {
	t := strings.ToLower(text)
	return strings.Contains(t, "无法生成更多图像") || strings.Contains(t, "unable to generate more images")
}

var sandboxHallucinationPatterns = []string{
	"I can run that for you",
	"I'll run that",
	"let me run that",
	"let me execute",
	"running in sandbox",
	"executing in sandbox",
	"code interpreter",
	"python sandbox",
	"sandbox environment",
	"linux container",
	"linux sandbox",
	"cloud sandbox",
	"execution environment has changed",
	"cannot access the Windows path",
	"only provides Linux",
	"只提供 Linux 容器",
	"no Windows execution",
	"don't have a Windows",
	"cannot execute on Windows",
	"no execution channel",
	"没有 Windows 执行通道",
	"没有执行通道",
	"cannot run commands on",
	"don't have command execution",
	"无法执行命令",
	"执行环境已经切换",
	"I don't have SSH access tools",
	"I don't have any tools",
	"none of which can reach",
}

// mountDataDenialPatterns describe the specific hallucination where the model
// claims the caller's workspace is a Linux mount (/mnt/data) that is empty or
// unavailable to the caller. They only fire together with an environment
// refusal so normal discussion of /mnt/data is not misclassified.
var mountDataDenialPatterns = []string{
	"/mnt/data 为空",
	"/mnt/data 是空的",
	"/mnt/data 也是空的",
	"/mnt/data is empty",
	"/mnt/data is not mounted",
	"/mnt/data has no",
	"no /mnt/data",
	"mnt/data empty",
	"cannot find /mnt/data",
	"/mnt/data 无法访问",
}

func isSandboxHallucination(text string) bool {
	low := strings.ToLower(text)
	for _, p := range sandboxHallucinationPatterns {
		if strings.Contains(low, strings.ToLower(p)) {
			return true
		}
	}
	if strings.Contains(low, "/mnt/data") {
		for _, p := range mountDataDenialPatterns {
			if strings.Contains(low, strings.ToLower(p)) {
				return true
			}
		}
	}
	return false
}

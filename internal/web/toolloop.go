package web

import (
	"encoding/json"
	"m365-copilot2api/internal/chathub"
	"os"
	"strings"
	"unicode/utf8"

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
			call.ID = callID()
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

func callID() string {
	return "call_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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

// promissoryOpeners are the phrases that open a promise of future action
// ("I will ...", "接下来我会 ..."). They head the third failure shape this
// gateway has had to guard, and the only one that survives every existing
// check: not a denial that tools exist (toolDenialPhrases), not a sandbox
// hallucination (sandboxHallucinationPatterns), but an execution request
// answered with a plan of what the model is about to do. 2026-09-11: the
// answering turn replied "我会把缺失的日期逻辑…一起补齐，然后用定向语法检查…
// 验收闭环。" with zero tool calls — it denies nothing, so every guard passed
// it through.
var promissoryOpeners = []string{
	"我会", "我将", "我将要", "我会先", "我先", "我先来", "接下来我", "下一步我",
	"下面我", "现在我将", "现在我会", "让我先", "让我来", "我将继续", "我会继续",
	"接着我", "然后我",
	"i will", "i'll", "i am going to", "i'm going to", "let me ", "next, i", "next i ",
}

// promissoryPlanMaxBytes bounds the shape. A real answer that merely opens
// with a promise ("我会从三个方面说明…") grows past this size and is released;
// a bare promise of upcoming work does not. The measured live case was 186
// bytes.
const promissoryPlanMaxBytes = 600

// promissoryEvidenceMarkers mark text that reports actual work. Their presence
// means the reply is not a bare promise, whatever it opens with.
var promissoryEvidenceMarkers = []string{
	"已完成", "已修改", "已修复", "已补齐", "已落地", "已更新", "已创建", "已删除",
	"执行结果", "运行结果", "命令输出",
	"already done", "has been updated", "i have already",
}

// promissoryHoldInitial is how many opening bytes stay undecided while the
// promissory hold is armed. Every opener is decided well before it — the
// longest is "i am going to" at 13 bytes — so a stream shorter than this has
// simply not said enough to judge yet.
const promissoryHoldInitial = 24

// isPromissoryPrefix reports whether s could still grow into a promissory
// opener once more deltas are appended — the same prefix-safety rule
// isProtocolMarkerPrefix enforces for the protocol marker. Stream deltas are
// tiny (the measured first delta is 4-5 bytes), so the opening fragment "我" or
// "I" must never be classified as "not a promise"; doing so released the hold
// and let the whole promise through, which is exactly how the 2026-09-11
// classification bug worked for CALL_TOOL.
//
// The case-folding is guarded by utf8.ValidString: a delta cut mid-rune makes
// strings.ToLower substitute U+FFFD, which changes the byte prefix and made
// "我\xe4" (the first 4 bytes of "我会…") fail to match. Byte-wise comparison of
// the raw string is what keeps a truncated opener detectable.
func isPromissoryPrefix(s string) bool {
	if s == "" {
		return false
	}
	valid := utf8.ValidString(s)
	low := s
	if valid {
		low = strings.ToLower(s)
	}
	for _, p := range promissoryOpeners {
		if strings.HasPrefix(p, s) || (valid && strings.HasPrefix(p, low)) {
			return true
		}
	}
	return false
}

// answerPromissoryGuardEnabled reports whether an execution request answered
// with a bare promise of future work is held back and retried, instead of being
// streamed as the final answer. Set M365_ANSWER_PROMISSORY_GUARD=0 to fall back
// to the previous behaviour.
func answerPromissoryGuardEnabled() bool {
	raw := strings.TrimSpace(os.Getenv("M365_ANSWER_PROMISSORY_GUARD"))
	if raw == "" {
		return true
	}
	return raw != "0" && !strings.EqualFold(raw, "false")
}

// isPromissoryPlan reports whether text is a bare promise of future work rather
// than the work itself: it opens with a promise phrase, is short enough to be
// just a promise, carries no code and no completion evidence. It is a shape
// detector, not a gate — callers pair it with userMessageDemandsAction so a
// conversational "我会建议…" answer is never touched.
func isPromissoryPlan(text string) bool {
	t := strings.TrimSpace(text)
	t = strings.TrimLeft(t, "*#>- \t\r\n")
	if t == "" {
		return false
	}
	low := strings.ToLower(t)
	matched := false
	for _, p := range promissoryOpeners {
		if strings.HasPrefix(low, p) {
			matched = true
			break
		}
	}
	if !matched {
		return false
	}
	if len(t) > promissoryPlanMaxBytes {
		return false
	}
	if strings.Contains(t, "```") {
		return false
	}
	for _, m := range promissoryEvidenceMarkers {
		if strings.Contains(low, strings.ToLower(m)) {
			return false
		}
	}
	return true
}

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

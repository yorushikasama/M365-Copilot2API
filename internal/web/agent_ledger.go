package web

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

type toolEvidence struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Result    string `json:"result"`
	Failed    bool   `json:"failed"`
}

type meteringSnapshot struct {
	MeterError         string         `json:"meterError,omitempty"`
	HasAccess          bool           `json:"hasAccess"`
	RemainingAllowance map[string]int `json:"remainingAllowance,omitempty"`
	Timestamp          time.Time      `json:"timestamp"`
}

type agentLedger struct {
	Completed           []toolEvidence     `json:"completed"`
	Pending             []toolEvidence     `json:"pending"`
	ToolRounds          int                `json:"tool_rounds"`
	RepeatedCall        bool               `json:"repeated_call"`
	RepeatedFailure     bool               `json:"repeated_failure"`
	RepetitionSignature string             `json:"repetition_signature,omitempty"`
	StuckLoop           bool               `json:"stuck_loop"`
	Metering            []meteringSnapshot `json:"metering,omitempty"`
}

var failureSignal = regexp.MustCompile(`(?i)(exit\s*(code|status)?\s*[:=]?\s*[1-9]\d*|\berror\b|\bfailed\b|\bfailure\b|exception|traceback|timed?\s*out|permission denied|not found|refused)`)
var unsupportedSuccess = regexp.MustCompile(`(?i)\b(installed|created|written|executed|ran|started|deployed|deleted|verified|completed|succeeded|successful(?:ly)?)\b`)

func compactToolResult(s string, limit int) string {
	s = strings.TrimSpace(s)
	if limit < 200 {
		limit = 200
	}
	if len(s) <= limit {
		return s
	}
	head := limit / 3
	tail := limit - head - 80
	if tail < 80 {
		tail = 80
	}
	return s[:head] + fmt.Sprintf("\n... [truncated %d bytes] ...\n", len(s)-head-tail) + s[len(s)-tail:]
}

// scopedCallID returns a globally unique tool call id. The scope parameters
// are kept for signature compatibility with callers that pass per-turn
// context; the id itself must not depend on call content or scope text,
// otherwise repeating the same tool+arguments across turns collides
// (duplicate tool call id errors from clients).
func scopedCallID(name, args string, index int, scope string) string {
	return "call_" + uuid.NewString()
}
func buildAgentLedger(messages []oaiMsg) agentLedger {
	calls := map[string]toolEvidence{}
	order := []string{}
	for _, m := range messages {
		if m.Role == "assistant" {
			for _, raw := range m.ToolCalls {
				id, _ := raw["id"].(string)
				fn, _ := raw["function"].(map[string]any)
				name, _ := fn["name"].(string)
				args := fmt.Sprint(fn["arguments"])
				if id != "" {
					calls[id] = toolEvidence{ID: id, Name: name, Arguments: args}
					order = append(order, id)
				}
			}
		}
		if m.Role == "tool" {
			if e, ok := calls[m.ToolCallID]; ok {
				e.Result = compactToolResult(contentToString(m.Content), 4000)
				e.Failed = failureSignal.MatchString(e.Result)
				calls[m.ToolCallID] = e
			}
		}
	}
	l := agentLedger{}
	seenCall := map[string]int{}
	seenFailure := map[string]int{}
	for _, id := range order {
		e := calls[id]
		l.ToolRounds++
		sig := e.Name + "\x00" + e.Arguments
		seenCall[sig]++
		if seenCall[sig] >= 2 {
			l.RepeatedCall = true
			l.RepetitionSignature = sig
		}
		if seenCall[sig] >= 3 {
			l.StuckLoop = true
		}
		if e.Result == "" {
			l.Pending = append(l.Pending, e)
		} else {
			l.Completed = append(l.Completed, e)
			if e.Failed {
				fs := e.Name + "\x00" + e.Arguments + "\x00" + normalizeFailure(e.Result)
				seenFailure[fs]++
				if seenFailure[fs] >= 2 {
					l.RepeatedFailure = true
					l.RepetitionSignature = fs
				}
				if seenFailure[fs] >= 3 {
					l.StuckLoop = true
				}
			}
		}
	}
	return l
}
func normalizeFailure(s string) string {
	s = strings.ToLower(s)
	s = regexp.MustCompile(`\d+`).ReplaceAllString(s, "#")
	if len(s) > 500 {
		s = s[:500]
	}
	return s
}
func (l agentLedger) RouterContext() string {
	// Bounded rendering: the ledger itself keeps every completed call for
	// duplicate filtering (hasCompleted/filterCompletedCalls), but rendering
	// the full history into the router prompt made the "slim" route prompt
	// grow without bound — 30 completed calls at up to 4KB of result each
	// produced ~120KB evidence blocks that dominated the prompt even after
	// router window slimming. Recent entries carry the current task state;
	// older ones collapse to a count.
	const (
		maxRenderedEntries = 10
		maxResultChars     = 300
	)
	completed := l.Completed
	omitted := 0
	if len(completed) > maxRenderedEntries {
		omitted = len(completed) - maxRenderedEntries
		completed = completed[len(completed)-maxRenderedEntries:]
	}
	rendered := make([]toolEvidence, len(completed))
	for i, e := range completed {
		e.Result = compactToolResult(e.Result, maxResultChars)
		rendered[i] = e
	}
	type compactView struct {
		Completed []toolEvidence `json:"completed"`
		Pending   []toolEvidence `json:"pending"`
		// OlderCompleted reports completed calls omitted from this view; they
		// remain final evidence and must not be re-issued.
		OlderCompleted int  `json:"older_completed,omitempty"`
		RepeatedCall   bool `json:"repeated_call,omitempty"`
	}
	view := compactView{Completed: rendered, Pending: l.Pending, OlderCompleted: omitted, RepeatedCall: l.RepeatedCall}
	hint := "Use only this compact evidence. A completed call is final evidence; do not issue the same name and arguments again."
	if l.RepeatedFailure {
		hint += " The same call failed repeatedly; change strategy instead of retrying unchanged."
	}
	if omitted > 0 {
		hint += fmt.Sprintf(" %d older completed calls are summarized by count only.", omitted)
	}
	return hint + "\nEVIDENCE_LEDGER: " + string(mustJSON(view))
}
func canonicalToolArguments(s string) string {
	s = strings.TrimSpace(s)
	var v any
	if json.Unmarshal([]byte(s), &v) == nil {
		b, _ := json.Marshal(v)
		return string(b)
	}
	return s
}

func (l agentLedger) hasCompleted(name, args string) bool {
	want := canonicalToolArguments(args)
	for _, e := range l.Completed {
		if e.Name == name && canonicalToolArguments(e.Arguments) == want {
			return true
		}
	}
	return false
}
// readOnlyToolNames are tools whose re-execution is harmless: their result can
// legitimately change between calls (a file may have been edited in between)
// and re-reading costs the user nothing.
//
// De-duplicating them by name+arguments was the single cause of a silent empty
// router decision observed 2026-09-10 01:14Z: the model asked to Read a file it
// had already read, the router dropped the call as "already completed", and the
// request fell through to the plain answer turn where the model — never told
// its call had been dropped — produced prose instead of the tool result.
var readOnlyToolNames = map[string]bool{
	"read": true, "readfile": true, "read_file": true, "readfiles": true,
	"notebookread": true, "notebook_read": true, "notebookreadoutput": true,
	"glob": true, "globfiles": true, "glob_files": true,
	"grep": true, "search": true, "searchcontent": true, "search_content": true,
	"codesearch": true, "code_search": true,
	"ls": true, "list": true, "listdir": true, "list_dir": true, "list_directory": true,
	"tree": true, "cat": true, "view": true, "show": true, "stat": true, "fileinfo": true,
	"webfetch": true, "web_fetch": true, "fetch": true, "websearch": true, "web_search": true,
	"get": true, "describe": true, "inspect": true, "gitstatus": true, "git_status": true,
}

// isRepeatableTool reports whether a tool may be issued again with identical
// arguments. Read-only inspection tools may; anything that mutates state
// (Write/Edit/Bash/...) stays de-duplicated, because repeating it is either
// wasted work or actively harmful.
func isRepeatableTool(name string) bool {
	key := strings.ToLower(strings.TrimSpace(name))
	key = strings.ReplaceAll(key, "-", "_")
	key = strings.ReplaceAll(key, " ", "_")
	if readOnlyToolNames[key] {
		return true
	}
	// Naming variants of the same operation (ReadManyFiles, MultiRead, ...).
	if strings.HasPrefix(key, "read") || strings.HasPrefix(key, "glob") ||
		strings.HasPrefix(key, "grep") || strings.HasPrefix(key, "list") {
		return true
	}
	return false
}

func filterCompletedCalls(calls []detectedToolCall, l agentLedger) []detectedToolCall {
	out := calls[:0]
	for _, c := range calls {
		// Repeatable (read-only) calls are never dropped: their result may have
		// changed, and dropping them silently degrades the turn into prose.
		if isRepeatableTool(c.Name) {
			out = append(out, c)
			continue
		}
		if !l.hasCompleted(c.Name, string(c.Arguments)) {
			out = append(out, c)
		}
	}
	return out
}

// duplicateCallNotice renders the calls the router de-duplicated away, together
// with the result they already produced, so the answer turn can use the real
// evidence instead of guessing. It is the safety net for the (now narrower) case
// where a non-repeatable call is dropped: without it the model is asked to
// answer a turn whose only tool decision vanished with no explanation.
func duplicateCallNotice(calls []detectedToolCall, l agentLedger) string {
	var b strings.Builder
	for _, c := range calls {
		want := canonicalToolArguments(string(c.Arguments))
		for _, e := range l.Completed {
			if e.Name != c.Name || canonicalToolArguments(e.Arguments) != want {
				continue
			}
			b.WriteString("\n[already-executed call] ")
			b.WriteString(c.Name)
			b.WriteString("(")
			b.WriteString(compactToolResult(string(c.Arguments), 400))
			b.WriteString(") was already executed in this session; its recorded result is final:\n")
			b.WriteString(compactToolResult(e.Result, 3000))
			b.WriteString("\nDo not repeat this call and do not claim you lack access to it. Use the recorded result, or choose different arguments/a different tool if new work genuinely remains.")
			break
		}
	}
	return b.String()
}
func recordMetering(l *agentLedger, meterError string, hasAccess bool, remaining map[string]int) {
	if l == nil {
		return
	}
	snap := meteringSnapshot{
		MeterError:         meterError,
		HasAccess:          hasAccess,
		RemainingAllowance: remaining,
		Timestamp:          time.Now(),
	}
	l.Metering = append(l.Metering, snap)
}

func (l agentLedger) CanContinue(maxRounds int) error {
	if maxRounds <= 0 {
		maxRounds = 32
	}
	if l.ToolRounds >= maxRounds {
		return fmt.Errorf("tool round limit reached: %d", maxRounds)
	}
	if l.StuckLoop {
		return fmt.Errorf("stuck tool loop detected: same call repeated 3+ times")
	}
	if l.RepeatedFailure {
		return fmt.Errorf("repeated tool failure detected: %s", l.RepetitionSignature)
	}
	if len(l.Pending) > 0 {
		return fmt.Errorf("pending tool results must be returned before another turn")
	}
	return nil
}
func maxToolRounds() int {
	if raw, ok := os.LookupEnv("M365_MAX_TOOL_ROUNDS"); ok {
		if n, e := strconv.Atoi(strings.TrimSpace(raw)); e == nil && n > 0 && n <= 512 {
			return n
		}
		return 32
	}
	if n := currentSettings().MaxToolRounds; n > 0 && n <= 512 {
		return n
	}
	return 32
}
func activeMessages(messages []oaiMsg) []oaiMsg {
	last := -1
	for i, m := range messages {
		if m.Role == "user" {
			last = i
		}
	}
	if last <= 0 {
		return messages
	}
	return messages[last:]
}
func completionEvidenceAllows(answer string, l agentLedger) bool {
	if len(l.Pending) > 0 {
		return false
	}
	if len(l.Completed) == 0 && len(l.Pending) == 0 {
		return !unsupportedSuccess.MatchString(answer)
	}
	low := strings.ToLower(answer)
	failureKeywords := []string{"cannot confirm", "not confirmed", "unable to confirm", "no tool result", "no matching tool results were returned", "no external action has been verified"}
	hasFailure := false
	for _, h := range failureKeywords {
		if strings.Contains(low, h) {
			hasFailure = true
			break
		}
	}
	if len(l.Completed) > 0 {
		return !hasFailure
	}
	if unsupportedSuccess.MatchString(answer) {
		return false
	}
	return true
}
func completedCallIDs(l agentLedger) []string {
	o := make([]string, 0, len(l.Completed))
	for _, e := range l.Completed {
		o = append(o, e.ID)
	}
	sort.Strings(o)
	return o
}

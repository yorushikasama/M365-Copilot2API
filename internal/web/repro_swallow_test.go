package web

import (
	"encoding/json"
	"strings"
	"testing"
)

// editTool is a realistic edit-tool declaration matching the production
// toolset: file_path (string), old_string (string), new_string (string),
// replace_all (boolean), sandbox_permissions (string enum), justification
// (string). The router renders these through routerToolCatalogue so the model
// sees name/desc/args; the full schema lives in the gateway and the model's
// emitted JSON is checked against it by validateDetectedToolCalls.
func editTool() []map[string]any {
	return []map[string]any{
		{"type": "function", "function": map[string]any{
			"name": "edit",
			"parameters": map[string]any{
				"type": "object",
				"required": []any{"file_path", "old_string", "new_string"},
				"properties": map[string]any{
					"file_path": map[string]any{"type": "string"},
					"old_string": map[string]any{"type": "string"},
					"new_string": map[string]any{"type": "string"},
					"replace_all": map[string]any{"type": "boolean"},
					"sandbox_permissions": map[string]any{"type": "string", "enum": []any{"use_default", "workspace_write", "workspace-write"}},
					"justification": map[string]any{"type": "string"},
				},
			},
		}},
		{"type": "function", "function": map[string]any{
			"name": "pwsh",
			"parameters": map[string]any{
				"type": "object",
				"required": []any{"command"},
				"properties": map[string]any{
					"command": map[string]any{"type": "string"},
					"workdir": map[string]any{"type": "string"},
					"timeoutMs": map[string]any{"type": "integer"},
					"sandbox_permissions": map[string]any{"type": "string"},
				},
			},
		}},
	}
}

// TestRouterModelEmittedWindowsPathSingleBackslash reproduces the 09-18 live
// swallows: the router model wrote a Windows path with single backslashes
// (D:\Word\odoo\...) inside CALL_TOOL: edit({...}). Single backslashes are not
// a valid JSON escape sequence, so json.Unmarshal of the argument object fails
// and parseModelToolDecision drops the call entirely (raw_calls=0), the router
// turn degrades into a repair attempt, and the user sees prose instead of a
// tool execution. The model's own decision was correct — the gateway parser
// could not read it.
func TestRouterModelEmittedWindowsPathSingleBackslash(t *testing.T) {
	// The path has \W \o \o (illegal escapes) and also \t in third_party
	// (a legal tab escape). Strict JSON rejects \W, which is what previously
	// swallowed the whole call; the repair must double the illegal escapes so
	// unmarshal succeeds. Legal escapes like \t are left untouched and so, per
	// the strict decoder, decode to their standard character — that is the
	// spec-compliant behaviour, not a repair defect.
	text := `CALL_TOOL: edit({"file_path":"D:\Word\odoo\third_party_addons\pos_sync_api\services\pos_sync_api.py","old_string":"            'date_order': fields.Datetime.now(),","new_string":"            'date_order': fields.Datetime.now(),  # UTC","replace_all":false,"sandbox_permissions":"workspace_write"})`
	calls, parsed := parseModelToolDecision(text, editTool(), "auto")
	if !parsed || len(calls) != 1 {
		t.Fatalf("model-emitted edit with Windows single-backslash path was swallowed: parsed=%v calls=%d", parsed, len(calls))
	}
	if calls[0].Name != "edit" {
		t.Fatalf("expected edit call, got %s", calls[0].Name)
	}
	var args struct {
		FilePath string `json:"file_path"`
	}
	if err := json.Unmarshal(calls[0].Arguments, &args); err != nil {
		t.Fatalf("cannot read arguments back: %v", err)
	}
	// The illegal escapes \W \o must survive as literal backslashes.
	if !strings.Contains(args.FilePath, `D:\Word\odoo`) {
		t.Fatalf("illegal escapes not preserved as literals: %q", args.FilePath)
	}
	if !strings.HasSuffix(args.FilePath, `pos_sync_api.py`) {
		t.Fatalf("file path tail lost: %q", args.FilePath)
	}
}

// TestRepairIllegalJSONEscapesPreservesLegalScapes pins the contract: legal
// JSON escapes (\n for newline, \\ for a literal backslash pair, \" for a
// quote inside a key value) must survive the repair byte-for-byte, because
// they carry meaning the model intended. Only illegal escapes are doubled.
func TestRepairIllegalJSONEscapesPreservesLegalScapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string // exact decoded value of the "v" field
	}{
		{"backslash_n_escaped", `{"v":"a\nb"}`, "a\nb"},
		{"escaped_backslash_pair", `{"v":"a\\b"}`, `a\b`},
		{"escaped_quote", `{"v":"say \"hi\""}`, `say "hi"`},
		{"unicode_escape", `{"v":"\u4f60\u597d"}`, "你好"},
		{"slash_escape", `{"v":"a\/b"}`, "a/b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repaired, changed := repairIllegalJSONEscapes([]byte(tc.in))
			var got struct {
				V string `json:"v"`
			}
			if err := json.Unmarshal(repaired, &got); err != nil {
				t.Fatalf("repaired JSON must parse: %v (repaired=%s)", err, repaired)
			}
			if got.V != tc.want {
				t.Fatalf("legal escape altered: got %q want %q (repaired=%s changed=%v)", got.V, tc.want, repaired, changed)
			}
		})
	}
}

// TestRepairIllegalJSONEscapesDoublesIllegalScapes verifies the actual repair
// on the live-swallowed shapes.
func TestRepairIllegalJSONEscapesDoublesIllegalScapes(t *testing.T) {
	// The live-swallowed shape: a Windows path where every backslash is a
	// path separator, i.e. an illegal escape. After repair the object must
	// parse and the path must keep its literal backslashes.
	in := `{"file_path":"D:\Word\odoo\pos\sync.py","workdir":"D:\Word"}`
	repaired, changed := repairIllegalJSONEscapes([]byte(in))
	if !changed {
		t.Fatalf("expected repair for %s, got none", in)
	}
	var out struct {
		FilePath string `json:"file_path"`
		Workdir  string `json:"workdir"`
	}
	if err := json.Unmarshal(repaired, &out); err != nil {
		t.Fatalf("repaired JSON must parse: %v (repaired=%s)", err, repaired)
	}
	if out.FilePath != `D:\Word\odoo\pos\sync.py` {
		t.Fatalf("path altered by repair: %q", out.FilePath)
	}
	if out.Workdir != `D:\Word` {
		t.Fatalf("workdir altered by repair: %q", out.Workdir)
	}
}

// TestRouterModelEmittedWindowsPathEscaped is the control: the same call with
// correctly escaped backslashes (D:\\Word\\odoo) must parse.
func TestRouterModelEmittedWindowsPathEscaped(t *testing.T) {
	text := `CALL_TOOL: edit({"file_path":"D:\\Word\\odoo\\third_party_addons\\pos_sync_api\\services\\pos_sync_api.py","old_string":"'date_order': fields.Datetime.now(),","new_string":"'date_order': fields.Datetime.now(),  # UTC","replace_all":false,"sandbox_permissions":"workspace_write"})`
	calls, parsed := parseModelToolDecision(text, editTool(), "auto")
	if !parsed || len(calls) != 1 {
		t.Fatalf("escaped Windows path must parse: parsed=%v calls=%d", parsed, len(calls))
	}
	if calls[0].Name != "edit" {
		t.Fatalf("expected edit call, got %s", calls[0].Name)
	}
}

// TestRouterModelEmittedPwshWithQuotedCommand reproduces the 09-18 05:35
// swallow: a pwsh call whose command string itself contains escaped quotes and
// a nested Windows path.
func TestRouterModelEmittedPwshWithQuotedCommand(t *testing.T) {
	text := `CALL_TOOL: pwsh({"command":"$ErrorActionPreference = 'Stop'; Write-Output \"PWD=$((Get-Location).Path)\"; git -C D:\Word\odoo status","sandbox_permissions":"use_default","timeoutMs":30000,"workdir":"D:\Word\odoo"})`
	calls, parsed := parseModelToolDecision(text, editTool(), "auto")
	if !parsed || len(calls) != 1 {
		t.Fatalf("model-emitted pwsh was swallowed: parsed=%v calls=%d", parsed, len(calls))
	}
	if calls[0].Name != "pwsh" {
		t.Fatalf("expected pwsh call, got %s", calls[0].Name)
	}
}

// TestRouterModelEmittedEditOldStringWithUnicode verifies old_string/new_string
// containing Chinese text (the 09-17 01:05 swallow had justification with
// Chinese and a long old_string) still parse.
func TestRouterModelEmittedEditOldStringWithUnicode(t *testing.T) {
	text := `CALL_TOOL: edit({"file_path":"third_party_addons/pos_sync_api/services/pos_sync_api.py","old_string":"    def _sync_order(self, order):\n        \"\"\"Sync a single order\"\"\"\n        self._log(\"syncing\", order.id)","new_string":"    def _sync_order(self, order):\n        \"\"\"Sync a single order\"\"\"\n        self._log(\"syncing (api)\", order.id)","sandbox_permissions":"workspace_write","justification":"按用户要求调整两个销售订单同步方法的注释格式。"})`
	calls, parsed := parseModelToolDecision(text, editTool(), "auto")
	if !parsed || len(calls) != 1 {
		t.Fatalf("model-emitted edit with unicode/newlines was swallowed: parsed=%v calls=%d", parsed, len(calls))
	}
	if calls[0].Name != "edit" {
		t.Fatalf("expected edit, got %s", calls[0].Name)
	}
	if !strings.Contains(string(calls[0].Arguments), "按用户要求") {
		t.Fatalf("justification was lost: %s", calls[0].Arguments)
	}
}

package web

import (
	"fmt"
	"log"
)

const missingToolResultPlaceholder = "[gateway] the client did not provide this tool result; it was lost or reordered in the caller's history. Treat the step as unverified and continue."

// repairToolConversation enforces the OpenAI tool protocol without making
// assumptions about what a tool does, but it HEALS instead of rejecting:
// real client histories (long Codex/ZCode sessions with hundreds of items)
// occasionally arrive with a tool result lost or reordered by client-side
// compaction. A hard 400 there kills the whole session — the client reports
// it as an instant empty model response — while a synthesized placeholder
// keeps the upstream protocol valid and lets the turn proceed.
//
// Returns the repaired slice (same backing array where possible) and an
// error only for genuinely unrecoverable input.
func repairToolConversation(requestID string, messages []oaiMsg) ([]oaiMsg, error) {
	if len(messages) > 0 {
		first := messages[0].Role
		if first != "system" && first != "developer" && first != "user" && first != "assistant" {
			return messages, fmt.Errorf("first message must have role system, developer, user, or assistant, got %q", first)
		}
	}
	pending := map[string]bool{}
	completed := map[string]bool{}
	repaired := false
	// Fresh backing array: appending while ranging over the input would
	// alias and overwrite not-yet-visited elements.
	out := make([]oaiMsg, 0, len(messages)+4)
	flushMissing := func() {
		if len(pending) == 0 {
			return
		}
		repaired = true
		for id := range pending {
			log.Printf("[tool-state] id=%s repair=missing_result tool_call_id=%s", requestID, id)
			out = append(out, oaiMsg{Role: "tool", ToolCallID: id, Content: missingToolResultPlaceholder})
			completed[id] = true
		}
		pending = map[string]bool{}
	}
	for _, m := range messages {
		switch m.Role {
		case "assistant":
			flushMissing()
			for _, call := range m.ToolCalls {
				id, _ := call["id"].(string)
				if id == "" {
					return messages, fmt.Errorf("assistant tool call missing id")
				}
				if pending[id] || completed[id] {
					// A repeated call id cannot be paired twice; drop the
					// duplicate call instead of failing the whole request.
					repaired = true
					log.Printf("[tool-state] id=%s repair=duplicate_call tool_call_id=%s", requestID, id)
					continue
				}
				pending[id] = true
			}
		case "tool":
			if m.ToolCallID == "" {
				repaired = true
				log.Printf("[tool-state] id=%s repair=drop_result_without_id", requestID)
				continue
			}
			if !pending[m.ToolCallID] {
				// Orphan result: the matching call is not in the client's
				// history (dropped by compaction). Keep it out of the upstream
				// payload; the upstream API rejects unpaired results.
				repaired = true
				log.Printf("[tool-state] id=%s repair=drop_orphan_result tool_call_id=%s", requestID, m.ToolCallID)
				continue
			}
			delete(pending, m.ToolCallID)
			completed[m.ToolCallID] = true
		}
		out = append(out, m)
	}
	// History ending with unresolved calls (client truncated mid-sequence):
	// close the protocol so the upstream sees a valid conversation.
	flushMissing()
	if repaired {
		log.Printf("[tool-state] id=%s repair=done messages_in=%d messages_out=%d", requestID, len(messages), len(out))
	}
	return out, nil
}

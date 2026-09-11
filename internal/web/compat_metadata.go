package web

import (
	"m365-copilot2api/internal/chathub"
	"os"
	"strings"
)

func envTrue(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func compatM365Metadata(res chathub.Result) map[string]any {
	m := map[string]any{
		"conversationId": res.ConversationID,
		"sessionId":      res.SessionID,
		"requestId":      res.RequestID,
		"usage_source":   "unavailable_from_chathub",
	}
	if res.Throttling != nil {
		m["throttling"] = res.Throttling
	}
	if len(res.SuggestedResponses) > 0 {
		m["suggestedResponses"] = sanitizedSuggestedResponses(res.SuggestedResponses)
	}
	if res.Offense != "" {
		m["offense"] = res.Offense
	}
	if len(res.Scores) > 0 {
		m["scores"] = res.Scores
	}
	if res.ConversationTransferToken != "" {
		m["conversationTransferToken"] = res.ConversationTransferToken
	}
	if res.MeteringInformation != nil {
		m["meteringInformation"] = res.MeteringInformation
	}
	if res.SpokenText != "" {
		m["spokenText"] = stripInternalCitationMarkers(res.SpokenText)
	}
	if res.Timestamps.RequestSent != "" {
		m["timestamps"] = res.Timestamps
	}
	if res.StorageMessageID != "" {
		m["storageMessageId"] = res.StorageMessageID
	}
	if len(res.References) > 0 {
		citations := make([]map[string]any, 0, len(res.References))
		for key, ref := range res.References {
			c := map[string]any{"key": key}
			if ref.TargetLink != "" {
				c["url"] = ref.TargetLink
			}
			if ref.Title != "" {
				c["title"] = ref.Title
			}
			if ref.Snippet != "" {
				c["snippet"] = ref.Snippet
			}
			if ref.ProviderDisplayName != "" {
				c["provider"] = ref.ProviderDisplayName
			}
			citations = append(citations, c)
		}
		m["citations"] = citations
	}
	if envTrue("M365_INCLUDE_UPSTREAM_EVENTS") {
		m["events"] = res.Events
	}
	return m
}

// sanitizedSuggestedResponses copies the follow-up suggestions with the upstream
// citation sentinels removed. These are model-authored prose the client renders
// as clickable chips, so a cited suggestion showed up as "citeturn2search1"
// glued to the text exactly like the assistant content did.
func sanitizedSuggestedResponses(in []chathub.SuggestedResponse) []chathub.SuggestedResponse {
	out := make([]chathub.SuggestedResponse, len(in))
	copy(out, in)
	for i := range out {
		out[i].Text = stripInternalCitationMarkers(out[i].Text)
		out[i].CommandText = stripInternalCitationMarkers(out[i].CommandText)
		out[i].HiddenText = stripInternalCitationMarkers(out[i].HiddenText)
	}
	return out
}

func normalizedToolChoiceMode(choice any) string {
	if choice == nil {
		return "auto"
	}
	if s, ok := choice.(string); ok {
		return s
	}
	if m, ok := choice.(map[string]any); ok {
		if f, ok := m["function"].(map[string]any); ok {
			if n, ok := f["name"].(string); ok {
				return "named:" + n
			}
		}
		if n, ok := m["name"].(string); ok {
			return "named:" + n
		}
	}
	return "auto"
}

package web

// Keep this conversion isolated so XML and native ChatHub events share the same
// OpenAI response shape. The event payload remains available under m365.
func toolCallMaps(calls []detectedToolCall) []any {
	out := make([]any, 0, len(calls))
	for _, c := range calls {
		typ := c.Type
		if typ == "" {
			typ = "function"
		}
		// Arguments carry model-authored prose (a plan, a commit message), so the
		// upstream citation sentinels land inside them too. Nothing downstream
		// strips them here: only assistant CONTENT went through the sanitizer, so
		// a cited argument reached the client as "citecall_<uuid>" — the sentinels
		// are invisible in a terminal, leaving just their payload glued to the text.
		out = append(out, map[string]any{"id": c.ID, "type": typ, "function": map[string]any{"name": c.Name, "arguments": stripInternalCitationMarkers(string(c.Arguments))}})
	}
	return out
}

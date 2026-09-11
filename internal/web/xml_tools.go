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
		out = append(out, map[string]any{"id": c.ID, "type": typ, "function": map[string]any{"name": c.Name, "arguments": string(c.Arguments)}})
	}
	return out
}

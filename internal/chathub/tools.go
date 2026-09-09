package chathub

import (
	"encoding/json"
)

type Tool struct {
	Type     string          `json:"type,omitempty"`
	Function json.RawMessage `json:"function,omitempty"`
}

// callerCanExecute reports whether this request carries execution capability
// on the caller's machine: caller-declared API tools.
//
// This is deliberately not len(clientPlugins(...)) > 0. clientPlugins falls
// back to a built-in BingWebSearch plugin when the request declares no tools
// at all, so "has plugins" is true even for an ordinary chat and cannot be
// used to tell a chat turn from an agent turn. Only API tools mean the caller
// can actually execute something, and only those should prime the model with
// the caller-side execution contract.
//
// The former second channel — a self-referential MCP gateway plugin — was
// removed on 2026-09-09: it exposed every declared tool twice (once as an API
// plugin, once as an MCP server tool), and the MCP transport could never
// execute anything because tool execution happens on the API client, so the
// upstream saw duplicate tools and emitted duplicate subtask calls.
func callerCanExecute(tools []Tool) bool {
	for _, t := range tools {
		var f struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(t.Function, &f) == nil && f.Name != "" {
			return true
		}
	}
	return false
}

func clientPlugins(tools []Tool) []any {
	plugins := make([]any, 0, len(tools)+1)
	if len(tools) == 0 {
		plugins = append(plugins, map[string]any{"Id": "BingWebSearch", "Source": "BuiltIn"})
	}
	for _, t := range tools {
		var f struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		}
		if json.Unmarshal(t.Function, &f) != nil || f.Name == "" {
			continue
		}
		plugins = append(plugins, map[string]any{"Id": f.Name, "Source": "API", "Description": f.Description, "Parameters": f.Parameters})
	}
	return plugins
}

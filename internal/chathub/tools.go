package chathub

import (
	"encoding/json"
	"strings"
)

type Tool struct {
	Type     string          `json:"type"`
	Function json.RawMessage `json:"function,omitempty"`
}

// callerCanExecute reports whether this request carries execution capability
// on the caller's machine: caller-declared API tools or an MCP gateway.
//
// This is deliberately not len(clientPlugins(...)) > 0. clientPlugins falls
// back to a built-in BingWebSearch plugin when the request declares no tools
// at all, so "has plugins" is true even for an ordinary chat and cannot be
// used to tell a chat turn from an agent turn. Only API tools and an MCP
// gateway mean the caller can actually execute something, and only those
// should prime the model with the caller-side execution contract.
func callerCanExecute(tools []Tool, mcpServerURL string) bool {
	if strings.TrimSpace(mcpServerURL) != "" {
		return true
	}
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

func clientPlugins(tools []Tool, mcpServerURL string) []any {
	plugins := make([]any, 0, len(tools)+2)
	if mcpServerURL == "" && len(tools) == 0 {
		plugins = append(plugins, map[string]any{"Id": "BingWebSearch", "Source": "BuiltIn"})
	}
	if mcpServerURL != "" {
		plugins = append(plugins, map[string]any{
			"Id":                "mcp-gateway",
			"Source":            "MCPServer",
			"Description":       "MCP Gateway tools",
			"Transport":         "mcp",
			"TransportUrl":      mcpServerURL,
			"TransportProtocol": "https://copilot.microsoft.com/schemas/plugins/local/transport/1.0",
		})
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

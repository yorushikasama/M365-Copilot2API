package mcp

import (
	"strings"
	"sync"
)

type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema,omitempty"`
}

type CallResult struct {
	Content        []map[string]any `json:"content,omitempty"`
	StructuredData any              `json:"structuredContent,omitempty"`
	IsError        bool             `json:"isError,omitempty"`
}

type ToolCache struct {
	mu    sync.RWMutex
	tools []Tool
}

func (c *ToolCache) Replace(tools []Tool) {
	copyTools := append([]Tool(nil), tools...)
	c.mu.Lock()
	c.tools = copyTools
	c.mu.Unlock()
}

func (c *ToolCache) List() []Tool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]Tool(nil), c.tools...)
}

func (r CallResult) Text() string {
	var out []string
	for _, block := range r.Content {
		if typ, _ := block["type"].(string); typ != "text" {
			continue
		}
		if text, _ := block["text"].(string); text != "" {
			out = append(out, text)
		}
	}
	return strings.Join(out, "\n")
}

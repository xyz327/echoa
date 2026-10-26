package mcp

import (
	"context"
	"fmt"
	"sync"

	"github.com/casdoor/casdoor-go-sdk/casdoorsdk"
)

type ToolHandler func(ctx context.Context, claims *casdoorsdk.Claims, args map[string]any) (string, error)

type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Handler     ToolHandler    `json:"-"`
}

type ToolRegistry struct {
	mu    sync.RWMutex
	tools map[string]*ToolDefinition
}

func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{tools: make(map[string]*ToolDefinition)}
}

func (r *ToolRegistry) Register(tool *ToolDefinition) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[tool.Name] = tool
}

func (r *ToolRegistry) ListTools() []*ToolDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	list := make([]*ToolDefinition, 0, len(r.tools))
	for _, t := range r.tools {
		list = append(list, t)
	}
	return list
}

func (r *ToolRegistry) Execute(ctx context.Context, claims *casdoorsdk.Claims, name string, args map[string]any) (string, error) {
	r.mu.RLock()
	tool, ok := r.tools[name]
	r.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("tool '%s' not found", name)
	}
	return tool.Handler(ctx, claims, args)
}

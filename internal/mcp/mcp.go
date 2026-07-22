// Package mcp abstracts MCP (Model Context Protocol) servers as tool
// sources for the agent. Client is implemented for real by SDKClient
// (wrapping the official MCP Go SDK) and by mcptest.Fake in tests. Manager
// aggregates several Clients (e.g. Home Assistant plus other tool servers)
// into one flat, collision-free tool namespace for the LLM.
package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/archer-developer/miranda/internal/llm"
)

// Client is one MCP server's tool source: it can list its tools and invoke
// them by name.
type Client interface {
	// Name is this server's short identifier, used as the tool name prefix.
	Name() string
	ListTools(ctx context.Context) ([]llm.ToolDef, error)
	// CallTool invokes tool name (unprefixed) with argumentsJSON and returns
	// the tool's textual result.
	CallTool(ctx context.Context, name string, argumentsJSON string) (string, error)
}

// Manager aggregates tools from multiple MCP Clients into one namespace,
// prefixing each tool name with "<server>_" so identically named tools from
// different servers can't collide (see PROJECT_PREREQUISITES.md's open
// question on tool naming conventions across multiple MCP sources).
type Manager struct {
	clients map[string]Client
	order   []string
}

// NewManager builds a Manager over the given Clients.
func NewManager(clients ...Client) *Manager {
	m := &Manager{clients: make(map[string]Client, len(clients))}
	for _, c := range clients {
		m.clients[c.Name()] = c
		m.order = append(m.order, c.Name())
	}
	return m
}

// Tools lists every tool across all configured servers, with names prefixed
// by their owning server so Call can route back to the right one.
func (m *Manager) Tools(ctx context.Context) ([]llm.ToolDef, error) {
	var out []llm.ToolDef
	for _, name := range m.order {
		tools, err := m.clients[name].ListTools(ctx)
		if err != nil {
			return nil, fmt.Errorf("mcp: list tools on %s: %w", name, err)
		}
		for _, t := range tools {
			t.Name = name + "_" + t.Name
			out = append(out, t)
		}
	}
	return out, nil
}

// Call routes a prefixed tool name (as produced by Tools) to the server that
// owns it and invokes it there.
func (m *Manager) Call(ctx context.Context, prefixedName, argumentsJSON string) (string, error) {
	for _, name := range m.order {
		prefix := name + "_"
		if strings.HasPrefix(prefixedName, prefix) {
			return m.clients[name].CallTool(ctx, strings.TrimPrefix(prefixedName, prefix), argumentsJSON)
		}
	}
	return "", fmt.Errorf("mcp: no configured server matches tool %q", prefixedName)
}

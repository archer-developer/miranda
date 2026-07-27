// Package mcptest provides a scriptable fake implementing mcp.Client, so
// unit and integration tests can exercise tool-calling through the agent
// loop without a real MCP server.
package mcptest

import (
	"context"
	"fmt"
	"sync"

	"github.com/archer-developer/miranda/internal/llm"
)

// FakeClient is a Client that serves a fixed set of tools and returns
// scripted results for calls, recording every call it receives.
type FakeClient struct {
	name    string
	tools   []llm.ToolDef
	listErr error // if set, ListTools fails with this instead of returning tools

	mu      sync.Mutex
	results map[string]string // tool name -> result to return
	errs    map[string]error  // tool name -> error to return instead
	Calls   []Call
	Closed  bool // set once Close is called, so tests can assert cleanup happened
}

// Call records one CallTool invocation the FakeClient received.
type Call struct {
	Tool          string
	ArgumentsJSON string
}

// New creates a FakeClient named name exposing tools.
func New(name string, tools ...llm.ToolDef) *FakeClient {
	return &FakeClient{
		name:    name,
		tools:   tools,
		results: make(map[string]string),
		errs:    make(map[string]error),
	}
}

// WithResult makes future calls to tool return result.
func (f *FakeClient) WithResult(tool, result string) *FakeClient {
	f.results[tool] = result
	return f
}

// WithError makes future calls to tool fail with err.
func (f *FakeClient) WithError(tool string, err error) *FakeClient {
	f.errs[tool] = err
	return f
}

// WithListToolsError makes ListTools fail with err instead of returning this
// FakeClient's tools — simulates a server that's unreachable or whose
// session has died.
func (f *FakeClient) WithListToolsError(err error) *FakeClient {
	f.listErr = err
	return f
}

func (f *FakeClient) Name() string { return f.name }

// Close implements Client. Safe to call more than once, like a real session.
func (f *FakeClient) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Closed = true
	return nil
}

func (f *FakeClient) ListTools(ctx context.Context) ([]llm.ToolDef, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.tools, nil
}

func (f *FakeClient) CallTool(ctx context.Context, name, argumentsJSON string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.Calls = append(f.Calls, Call{Tool: name, ArgumentsJSON: argumentsJSON})

	if err, ok := f.errs[name]; ok {
		return "", err
	}
	if result, ok := f.results[name]; ok {
		return result, nil
	}
	return "", fmt.Errorf("mcptest: no scripted result for tool %q on %q", name, f.name)
}

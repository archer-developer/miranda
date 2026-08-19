// Package mcptest provides a scriptable fake implementing mcp.Client, so
// unit and integration tests can exercise tool-calling through the agent
// loop without a real MCP server.
package mcptest

import (
	"context"
	"fmt"
	"sync"

	llm "github.com/archer-developer/miranda-llm"
)

// FakeClient is a Client that serves a fixed set of tools and returns
// scripted results for calls, recording every call it receives.
//
// listErr and listToolsBlock are set only through the builder chain before
// a FakeClient is handed to a Manager (every current call site does this) —
// unlike Calls/Closed below, they're read from ListTools without their own
// lock. That's only safe as long as construction stays single-threaded; a
// test that calls WithListToolsError/WithListToolsBlockingOn on an
// already-shared FakeClient would race with a concurrent ListTools.
type FakeClient struct {
	name           string
	tools          []llm.ToolDef
	listErr        error           // if set, ListTools fails with this instead of returning tools
	listToolsBlock <-chan struct{} // if set, ListTools blocks until this closes or ctx is done

	mu      sync.Mutex
	results map[string]string // tool name -> result to return
	errs    map[string]error  // tool name -> error to return instead
	Calls   []Call
	Closed  bool // set once Close is called; read it via IsClosed(), not directly
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

// WithListToolsBlockingOn makes ListTools block until either ctx passed to
// it is done (returning ctx.Err() unwrapped — same shape SDKClient's real
// ListTools returns before classifyErr wraps it) or unblock closes
// (returning this FakeClient's tools normally) — simulates a server that's
// merely slow to answer rather than genuinely disconnected, for tests that
// need to exercise a caller's own timeout handling distinctly from a real
// disconnection.
func (f *FakeClient) WithListToolsBlockingOn(unblock <-chan struct{}) *FakeClient {
	f.listToolsBlock = unblock
	return f
}

// Name implements Client.
func (f *FakeClient) Name() string { return f.name }

// Close implements Client. Safe to call more than once, like a real session.
func (f *FakeClient) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Closed = true
	return nil
}

// IsClosed reports whether Close has been called — the synchronized way to
// read the Closed field from a test that runs concurrently with the code
// under test, rather than reading it directly (see FakeClient's own doc
// comment on why a bare field read isn't generally safe).
func (f *FakeClient) IsClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Closed
}

// ListTools implements Client.
func (f *FakeClient) ListTools(ctx context.Context) ([]llm.ToolDef, error) {
	if f.listToolsBlock != nil {
		select {
		case <-ctx.Done():
			// If the caller also configured listErr, that's what a real
			// SDKClient would actually surface here too — classifyErr wraps
			// ctx's own DeadlineExceeded the same as any other non-RPC
			// error, so a test simulating "real disconnection, discovered
			// only after some delay" should get that same wrapped error
			// back, not a bare ctx.Err() the real Client never produces.
			if f.listErr != nil {
				return nil, f.listErr
			}
			return nil, ctx.Err()
		case <-f.listToolsBlock:
		}
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.tools, nil
}

// CallTool implements Client.
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

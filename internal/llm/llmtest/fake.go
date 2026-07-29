// Package llmtest provides a scriptable fake implementing llm.Provider, used
// by unit tests and the full agent-loop integration test to exercise router
// fallback/escalation and tool-calling without any real network/LLM backend.
package llmtest

import (
	"context"
	"fmt"
	"sync"

	"github.com/archer-developer/miranda/internal/llm"
)

// Response is one scripted reply the fake Provider will emit, in order.
type Response struct {
	// Text, if non-empty, is streamed as a single StreamChunk.TextDelta.
	Text string
	// ToolCall, if set, is emitted as a StreamChunk.ToolCall after Text.
	ToolCall *llm.ToolCall
	// Err, if set, is returned as an immediate error from Chat (nothing streamed).
	Err error
	// StreamErr, if set, is delivered as a StreamChunk.Err from an
	// otherwise-successful Chat call — unlike Err, this models how every
	// real provider (internal/llm/anthropic, internal/llm/gemini,
	// internal/llm/openaicompat) actually reports a failure: Chat itself
	// returns (stream, nil) and streams the error later from its pump
	// goroutine, e.g. after internal/keyrotation exhausts every configured
	// API key across every retry cycle. Router.deliver's escalation-on-error
	// path only ever sees this shape in production, not Err.
	StreamErr error
}

// FakeProvider replays a fixed script of Responses, one per call to Chat, and
// records every request it received so tests can assert on router/orchestrator
// behavior.
type FakeProvider struct {
	name string

	mu       sync.Mutex
	script   []Response
	callIdx  int
	Requests []llm.ChatRequest
	tracer   llm.Tracer
}

// New creates a FakeProvider named name that will return script[i] on the
// i-th call to Chat. Calling Chat more times than len(script) panics, since
// that indicates the test under-specified expected behavior.
func New(name string, script ...Response) *FakeProvider {
	return &FakeProvider{name: name, script: script}
}

func (f *FakeProvider) Name() string { return f.name }

// SetTracer implements the same optional tracing hook the real providers
// (internal/llm/anthropic, internal/llm/openaicompat) expose, so tests can
// exercise internal/llm/router.Router's tracer-forwarding without a real
// SDK — see Chat, which calls it exactly like a real provider would.
func (f *FakeProvider) SetTracer(t llm.Tracer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tracer = t
}

// Chat implements llm.Provider.
func (f *FakeProvider) Chat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, error) {
	f.mu.Lock()
	if f.callIdx >= len(f.script) {
		f.mu.Unlock()
		panic(fmt.Sprintf("llmtest: FakeProvider %q got more Chat calls (%d) than scripted responses (%d)", f.name, f.callIdx+1, len(f.script)))
	}
	resp := f.script[f.callIdx]
	f.callIdx++
	f.Requests = append(f.Requests, req)
	tracer := f.tracer
	f.mu.Unlock()

	if resp.Err != nil {
		if tracer != nil {
			tracer.Trace(ctx, f.name, fmt.Sprintf("%+v", req), "", resp.Err)
		}
		return nil, resp.Err
	}

	if tracer != nil {
		response := resp.Text
		if resp.ToolCall != nil {
			response += fmt.Sprintf(" tool_call:%s(%s)", resp.ToolCall.Name, resp.ToolCall.Arguments)
		}
		tracer.Trace(ctx, f.name, fmt.Sprintf("%+v", req), response, resp.StreamErr)
	}

	if resp.StreamErr != nil {
		ch := make(chan llm.StreamChunk, 1)
		ch <- llm.StreamChunk{Err: resp.StreamErr}
		close(ch)
		return ch, nil
	}

	ch := make(chan llm.StreamChunk, 3)
	if resp.Text != "" {
		ch <- llm.StreamChunk{TextDelta: resp.Text}
	}
	if resp.ToolCall != nil {
		ch <- llm.StreamChunk{ToolCall: resp.ToolCall}
	}
	ch <- llm.StreamChunk{Done: true}
	close(ch)
	return ch, nil
}

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
}

// New creates a FakeProvider named name that will return script[i] on the
// i-th call to Chat. Calling Chat more times than len(script) panics, since
// that indicates the test under-specified expected behavior.
func New(name string, script ...Response) *FakeProvider {
	return &FakeProvider{name: name, script: script}
}

func (f *FakeProvider) Name() string { return f.name }

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
	f.mu.Unlock()

	if resp.Err != nil {
		return nil, resp.Err
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

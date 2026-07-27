// Package router selects among configured llm.Providers: it retries an
// ordered fallback chain when a provider is unreachable, and transparently
// reroutes a turn to the escalation target when the active provider invokes
// the configured escalation tool (e.g. a small local model calling
// escalate_to_claude on a hard question).
package router

import (
	"context"
	"fmt"

	"github.com/archer-developer/miranda/internal/config"
	"github.com/archer-developer/miranda/internal/llm"
)

// Router dispatches chat turns to the configured llm.Providers.
type Router struct {
	providers  map[string]llm.Provider
	order      []string // fallback chain, in the order providers were configured
	escalation config.EscalationConfig
}

// New builds a Router over providers, tried in the given slice order for
// reliability fallback (first provider is the normal/default one).
func New(providers []llm.Provider, escalation config.EscalationConfig) (*Router, error) {
	if len(providers) == 0 {
		return nil, fmt.Errorf("router: no providers configured")
	}

	m := make(map[string]llm.Provider, len(providers))
	order := make([]string, 0, len(providers))
	for _, p := range providers {
		m[p.Name()] = p
		order = append(order, p.Name())
	}

	return &Router{providers: m, order: order, escalation: escalation}, nil
}

// traceable is implemented by providers that accept a request/response
// tracer directly (see llm.Tracer). Each Provider owns producing its own
// trace content — the exact request/response it built for its own SDK (see
// internal/llm/anthropic, internal/llm/openaicompat) — rather than the
// Router formatting one from the provider-agnostic ChatRequest, which
// can't represent provider-specific specifics like Anthropic's own
// server-side web_search/web_fetch/code_execution tools.
type traceable interface {
	SetTracer(t llm.Tracer)
}

// SetTracer wires up request/response tracing (see internal/llmtrace) by
// forwarding tracer to every configured provider that accepts one —
// optional, and left unset by default so tests and any caller that doesn't
// care about it don't need to touch this.
func (r *Router) SetTracer(tracer llm.Tracer) {
	for _, p := range r.providers {
		if t, ok := p.(traceable); ok {
			t.SetTracer(tracer)
		}
	}
}

// Chat runs req through the fallback chain and, if escalation triggers,
// reroutes to the escalation target mid-stream. onProviderUsed, if non-nil,
// is called exactly once with the name of the provider whose text ultimately
// reached the caller — used for the API's provider_used field and for log
// events published to the hub.
func (r *Router) Chat(ctx context.Context, req llm.ChatRequest, onProviderUsed func(name string)) (<-chan llm.StreamChunk, error) {
	stream, providerName, err := r.startChat(ctx, req)
	if err != nil {
		return nil, err
	}

	out := make(chan llm.StreamChunk)
	go r.pump(ctx, req, stream, providerName, out, onProviderUsed)
	return out, nil
}

// startChat tries each provider in order until one accepts the request,
// implementing reliability fallback for connection/auth failures.
func (r *Router) startChat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, string, error) {
	var lastErr error
	for _, name := range r.order {
		stream, err := r.providers[name].Chat(ctx, req)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", name, err)
			continue
		}
		return stream, name, nil
	}
	return nil, "", fmt.Errorf("router: all providers failed: %w", lastErr)
}

// pump forwards chunks from the active provider's stream to out, intercepting
// a call to the escalation tool to reroute the rest of the turn to the
// escalation target provider instead of surfacing that tool call to the
// caller. Tracing is not this function's concern: each Provider traces its
// own call internally (see SetTracer above) as soon as its own stream
// finishes, before pump ever sees the chunks.
func (r *Router) pump(ctx context.Context, req llm.ChatRequest, stream <-chan llm.StreamChunk, providerName string, out chan<- llm.StreamChunk, onProviderUsed func(string)) {
	defer close(out)

	reported := false
	report := func(name string) {
		if !reported && onProviderUsed != nil {
			onProviderUsed(name)
			reported = true
		}
	}

	for chunk := range stream {
		if chunk.Err != nil {
			out <- chunk
			return
		}

		if chunk.ToolCall != nil && r.escalation.Enabled && chunk.ToolCall.Name == r.escalation.ToolName {
			r.escalate(ctx, req, *chunk.ToolCall, out, report)
			return
		}

		if chunk.Done {
			report(providerName)
		}
		out <- chunk
	}
}

// escalate re-issues the conversation to the escalation target provider,
// with a synthetic tool result acknowledging the handoff, and forwards its
// stream as the rest of this turn's output. The target's own Chat call
// traces itself, the same as any other call to it.
func (r *Router) escalate(ctx context.Context, req llm.ChatRequest, call llm.ToolCall, out chan<- llm.StreamChunk, report func(string)) {
	target, ok := r.providers[r.escalation.TargetProvider]
	if !ok {
		err := fmt.Errorf("router: escalation target %q not configured", r.escalation.TargetProvider)
		out <- llm.StreamChunk{Err: err}
		return
	}

	escReq := appendEscalationTurn(req, call)
	escStream, err := target.Chat(ctx, escReq)
	if err != nil {
		wrapped := fmt.Errorf("router: escalation to %s failed: %w", target.Name(), err)
		out <- llm.StreamChunk{Err: wrapped}
		return
	}

	for c := range escStream {
		if c.Done {
			report(target.Name())
		}
		out <- c
	}
}

// appendEscalationTurn appends the assistant's escalation tool call and a
// synthetic tool result to the conversation, so the target provider sees a
// consistent history rather than a dangling unanswered tool call.
func appendEscalationTurn(req llm.ChatRequest, call llm.ToolCall) llm.ChatRequest {
	messages := make([]llm.Message, len(req.Messages), len(req.Messages)+2)
	copy(messages, req.Messages)
	messages = append(messages,
		llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{call}},
		llm.Message{Role: llm.RoleTool, ToolCallID: call.ID, Content: "escalated to a more capable model"},
	)
	return llm.ChatRequest{Messages: messages, Tools: req.Tools}
}

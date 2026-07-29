// Package router selects among configured llm.Providers: it retries an
// ordered fallback chain when a provider is unreachable, and transparently
// reroutes a turn to a provider's own configured escalation target when
// that provider invokes its escalation tool (e.g. a small local model
// calling escalate_to_gemini_strong on a hard question) — walking a chain
// of these hop by hop, since each provider can configure its own target
// (see config.LLMProvider.Escalation). A provider that hard-fails mid-turn
// (e.g. a Gemini provider that exhausted every configured API key's quota
// across every retry cycle before streaming a single chunk — see
// internal/llm/gemini and internal/keyrotation) triggers that same
// escalation target too, not just an explicit tool call (see deliver's use
// of escalateOnError) — a provider outage shouldn't fail a turn a
// configured stronger fallback could still answer.
package router

import (
	"context"
	"fmt"

	"github.com/archer-developer/miranda/internal/config"
	"github.com/archer-developer/miranda/internal/llm"
)

// maxEscalationHops caps how many hops one turn's escalation chain can walk
// — a real ladder is 2-3 hops deep (e.g. a cheap model escalates to a
// stronger one, which escalates to Claude); this exists purely to stop a
// misconfigured cycle (A escalates to B, B escalates back to A) from
// looping forever, not to cap a legitimate deep chain.
const maxEscalationHops = 6

// Router dispatches chat turns to the configured llm.Providers.
type Router struct {
	providers   map[string]llm.Provider
	order       []string                           // fallback chain, in the order providers were configured (or reordered by defaultProvider)
	escalations map[string]config.EscalationConfig // keyed by provider name; a name absent (or present with Enabled: false) has no escalation
}

// New builds a Router over providers, tried in the given slice order for
// reliability fallback, and escalations (one config.EscalationConfig per
// provider name, typically built from the same []config.LLMProvider used to
// construct providers — see cmd/miranda/main.go's buildEscalations).
// defaultProvider, if non-empty, is moved to the front of the fallback
// order — config.LLMConfig.DefaultProvider is the source of truth for
// "default", not Providers' list position. Errors if defaultProvider is
// set but doesn't match any configured provider's Name().
func New(providers []llm.Provider, escalations map[string]config.EscalationConfig, defaultProvider string) (*Router, error) {
	if len(providers) == 0 {
		return nil, fmt.Errorf("router: no providers configured")
	}

	m := make(map[string]llm.Provider, len(providers))
	order := make([]string, 0, len(providers))
	for _, p := range providers {
		m[p.Name()] = p
		order = append(order, p.Name())
	}

	if defaultProvider != "" {
		if _, ok := m[defaultProvider]; !ok {
			return nil, fmt.Errorf("router: default_provider %q not found among configured providers", defaultProvider)
		}
		order = moveToFront(order, defaultProvider)
	}

	return &Router{providers: m, order: order, escalations: escalations}, nil
}

// moveToFront returns order with name moved to index 0, preserving the
// relative order of everything else — the rest of order becomes the
// fallback chain behind the explicit default.
func moveToFront(order []string, name string) []string {
	out := make([]string, 0, len(order))
	out = append(out, name)
	for _, n := range order {
		if n != name {
			out = append(out, n)
		}
	}
	return out
}

// traceable is implemented by providers that accept a request/response
// tracer directly (see llm.Tracer). Each Provider owns producing its own
// trace content — the exact request/response it built for its own SDK (see
// internal/llm/anthropic, internal/llm/openaicompat, internal/llm/gemini)
// rather than the Router formatting one from the provider-agnostic
// ChatRequest, which can't represent provider-specific specifics like
// Anthropic's own server-side web_search/web_fetch/code_execution tools.
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

// defaultEscalationDescription is used when a provider's EscalationConfig
// doesn't set its own Description — generic enough for any provider, but
// see that field's doc comment for when a provider-specific one (e.g.
// naming a concrete missing capability like code execution) is worth
// setting instead.
const defaultEscalationDescription = "Hand this turn off to a more capable model when the request is too complex, ambiguous, or high-stakes for you to handle well."

// escalationToolDef builds the llm.ToolDef the active provider sees for its
// own configured escalation target. Built here, not by the Orchestrator,
// because each provider's escalation config can now differ (see
// config.LLMProvider.Escalation) and the Orchestrator has no per-provider
// visibility into which one is currently active at any given hop.
func escalationToolDef(esc config.EscalationConfig) llm.ToolDef {
	description := esc.Description
	if description == "" {
		description = defaultEscalationDescription
	}
	return llm.ToolDef{
		Name:        esc.ToolName,
		Description: description,
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"reason": map[string]any{"type": "string"}},
		},
	}
}

// requestFor returns req with esc's own escalation tool appended, if
// enabled — req itself (the base tool list) is never mutated, so each hop
// starts from the same base and only its own tool differs.
func requestFor(req llm.ChatRequest, esc config.EscalationConfig) llm.ChatRequest {
	if !esc.Enabled {
		return req
	}
	out := req
	out.Tools = append(append([]llm.ToolDef{}, req.Tools...), escalationToolDef(esc))
	return out
}

// Chat runs req through the fallback chain and, if escalation triggers,
// reroutes to the escalation target mid-stream — walking a chain of any
// depth (see deliver/escalate). onProviderUsed, if non-nil, is called
// exactly once with the name of the provider whose text ultimately reached
// the caller — used for the API's provider_used field and for log events
// published to the hub.
func (r *Router) Chat(ctx context.Context, req llm.ChatRequest, onProviderUsed func(name string)) (<-chan llm.StreamChunk, error) {
	stream, providerName, err := r.startChat(ctx, req)
	if err != nil {
		return nil, err
	}

	out := make(chan llm.StreamChunk)
	report := newOnceReporter(onProviderUsed)
	go r.pump(ctx, req, stream, providerName, out, report, map[string]bool{providerName: true})
	return out, nil
}

// newOnceReporter wraps onProviderUsed (may be nil) so it fires at most
// once regardless of how many escalation hops one turn walks.
func newOnceReporter(onProviderUsed func(string)) func(string) {
	var reported bool
	return func(name string) {
		if !reported && onProviderUsed != nil {
			onProviderUsed(name)
			reported = true
		}
	}
}

// startChat tries each provider in order until one accepts the request,
// implementing reliability fallback for connection/auth failures. Each
// candidate sees its own configured escalation tool appended to req.Tools
// (see requestFor), not a global one.
func (r *Router) startChat(ctx context.Context, req llm.ChatRequest) (<-chan llm.StreamChunk, string, error) {
	var lastErr error
	for _, name := range r.order {
		stream, err := r.providers[name].Chat(ctx, requestFor(req, r.escalations[name]))
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", name, err)
			continue
		}
		return stream, name, nil
	}
	return nil, "", fmt.Errorf("router: all providers failed: %w", lastErr)
}

// pump is the entry point for one Chat() call's whole output: it closes out
// exactly once, when the entire chain (initial call plus any escalation
// hops) finishes — deliver is the recursive part escalate calls back into,
// which must never close out itself.
func (r *Router) pump(ctx context.Context, req llm.ChatRequest, stream <-chan llm.StreamChunk, providerName string, out chan<- llm.StreamChunk, report func(string), visited map[string]bool) {
	defer close(out)
	r.deliver(ctx, req, stream, providerName, out, report, visited)
}

// deliver forwards chunks from the active provider's stream to out,
// intercepting a call to THAT provider's own configured escalation tool
// (r.escalations[providerName]) and recursing into escalate for the next
// hop instead of surfacing the tool call to the caller. Every hop's stream
// is examined the same way, which is what makes a chain of any depth work
// generically — not just the first hop, unlike the old flat single-hop
// forward. Tracing is not this function's concern: each Provider traces its
// own call internally (see SetTracer above) as soon as its own stream
// finishes, before deliver ever sees the chunks.
func (r *Router) deliver(ctx context.Context, req llm.ChatRequest, stream <-chan llm.StreamChunk, providerName string, out chan<- llm.StreamChunk, report func(string), visited map[string]bool) {
	esc := r.escalations[providerName]
	for chunk := range stream {
		if chunk.Err != nil {
			// A hard failure (e.g. a quota-exhausted Gemini provider that
			// burned through every configured key and retry cycle before
			// producing a single chunk) is treated the same as this
			// provider explicitly asking to escalate — same target, same
			// hop-cap/cycle guards in escalate — so a provider outage
			// doesn't fail turns a configured stronger fallback could
			// still answer. Only tried once per target (visited): if it's
			// already been visited this turn, escalating again would just
			// trade this real error for escalate's own cycle-detected one,
			// so fall through and surface the original failure instead.
			if esc.Enabled && !visited[esc.TargetProvider] {
				r.escalateOnError(ctx, req, chunk.Err, esc, out, report, visited)
				return
			}
			out <- chunk
			return
		}

		if chunk.ToolCall != nil && esc.Enabled && chunk.ToolCall.Name == esc.ToolName {
			r.escalate(ctx, req, *chunk.ToolCall, esc, out, report, visited)
			return
		}

		if chunk.Done {
			report(providerName)
		}
		out <- chunk
	}
}

// escalate re-issues the conversation (base req plus a synthetic tool-call/
// tool-result turn acknowledging the handoff) to esc's target provider, and
// hands its stream to deliver for the NEXT hop — recursing through the same
// interception logic rather than forwarding raw, so the target's own
// escalation tool call (if it has one configured) is caught too. Guards
// against runaway/cyclic chains with both a hop cap and a per-turn visited
// set (a provider appearing twice in one chain is always a
// misconfiguration — a legitimate ladder is a straight line, never a
// cycle). The hop count is exactly len(visited): Chat seeds visited with
// the one initial provider, and every call here that survives both guards
// adds exactly one new entry before recursing — so there's no separate hop
// counter to carry (and risk desyncing from visited) as a parameter.
func (r *Router) escalate(ctx context.Context, req llm.ChatRequest, call llm.ToolCall, esc config.EscalationConfig, out chan<- llm.StreamChunk, report func(string), visited map[string]bool) {
	if len(visited) >= maxEscalationHops {
		out <- llm.StreamChunk{Err: fmt.Errorf("router: escalation chain exceeded %d hops (target %q) — check for a configuration cycle", maxEscalationHops, esc.TargetProvider)}
		return
	}
	if visited[esc.TargetProvider] {
		out <- llm.StreamChunk{Err: fmt.Errorf("router: escalation cycle detected: %q already appeared earlier in this turn's chain", esc.TargetProvider)}
		return
	}
	target, ok := r.providers[esc.TargetProvider]
	if !ok {
		out <- llm.StreamChunk{Err: fmt.Errorf("router: escalation target %q not configured", esc.TargetProvider)}
		return
	}

	escReq := appendEscalationTurn(req, call)
	escStream, err := target.Chat(ctx, requestFor(escReq, r.escalations[target.Name()]))
	if err != nil {
		out <- llm.StreamChunk{Err: fmt.Errorf("router: escalation to %s failed: %w", target.Name(), err)}
		return
	}

	nextVisited := make(map[string]bool, len(visited)+1)
	for k := range visited {
		nextVisited[k] = true
	}
	nextVisited[target.Name()] = true

	r.deliver(ctx, escReq, escStream, target.Name(), out, report, nextVisited)
}

// escalateOnError is deliver's counterpart to an explicit escalation tool
// call, for a provider that hard-failed instead: it synthesizes a tool
// call/result turn just like a real one (so the target sees a consistent
// history, and escalate's own hop-cap/cycle guards apply identically) with
// providerErr's message standing in for the model's own stated reason.
func (r *Router) escalateOnError(ctx context.Context, req llm.ChatRequest, providerErr error, esc config.EscalationConfig, out chan<- llm.StreamChunk, report func(string), visited map[string]bool) {
	call := llm.ToolCall{
		ID:        "error-escalation",
		Name:      esc.ToolName,
		Arguments: fmt.Sprintf(`{"reason":"previous provider failed: %s"}`, providerErr.Error()),
	}
	r.escalate(ctx, req, call, esc, out, report, visited)
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

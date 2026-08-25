package redact

import (
	"context"

	llm "github.com/archer-developer/miranda-llm"
)

// Tracer masks an LLM call's request and response dumps before passing them
// on. It exists because logs/llm.log is by far the leakiest sink in Miranda:
// every provider traces json.MarshalIndent of the *entire* SDK request, which
// means each call writes the whole conversation so far, the system prompt
// with the user's memory folded into it, and the body of every text
// attachment inlined by processAttachments.
//
// Wrap the outermost tracer, not llmtrace.ContextTracer's Default — that one
// call also fans out to the per-turn anomaly.Recorder attached via ctx, so
// masking above it is what keeps logs/anomalies/ clean too. See
// cmd/miranda.run for the wiring.
//
// request and response are marshalled JSON. maskChar is a single JSON-safe
// ASCII byte and masking preserves the value's rune length, so a masked dump
// is still valid JSON — redact_test.go asserts this rather than trusting it.
type Tracer struct {
	// Next receives the masked call. Required.
	Next llm.Tracer
	// Redactor may be nil, in which case this wrapper is a pass-through.
	Redactor *Redactor
}

// Trace implements llm.Tracer.
func (t *Tracer) Trace(ctx context.Context, provider, request, response string, err error) {
	if t == nil || t.Next == nil {
		return
	}
	t.Next.Trace(ctx, provider, t.Redactor.Redact(request), t.Redactor.Redact(response), err)
}

package redact

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// recordingTracer captures what a wrapped tracer was handed, so the test can
// assert on what would have reached logs/llm.log.
type recordingTracer struct {
	provider string
	request  string
	response string
	err      error
	calls    int
}

func (r *recordingTracer) Trace(_ context.Context, provider, request, response string, err error) {
	r.provider, r.request, r.response, r.err = provider, request, response, err
	r.calls++
}

func TestTracer_MasksBeforeDelegating(t *testing.T) {
	next := &recordingTracer{}
	tracer := &Tracer{Next: next, Redactor: newTestRedactor(t)}

	request := `{"messages":[{"role":"user","content":"пин-код от телефона Ани 665533"}]}`
	response := `{"content":[{"text":"записала, пин-код 665533"}]}`

	tracer.Trace(context.Background(), "anthropic", request, response, nil)

	if next.calls != 1 {
		t.Fatalf("Next.Trace called %d times, want 1", next.calls)
	}
	if strings.Contains(next.request, "665533") {
		t.Errorf("pin reached the trace request: %s", next.request)
	}
	if strings.Contains(next.response, "665533") {
		t.Errorf("pin reached the trace response: %s", next.response)
	}
	if !strings.Contains(next.request, "пин-код от телефона Ани ******") {
		t.Errorf("surrounding text should survive: %s", next.request)
	}
	if next.provider != "anthropic" {
		t.Errorf("provider = %q, want %q", next.provider, "anthropic")
	}
}

// TestTracer_PassesErrorThrough — a failed call still gets a trace block, and
// the error must not be swallowed by the wrapper.
func TestTracer_PassesErrorThrough(t *testing.T) {
	next := &recordingTracer{}
	tracer := &Tracer{Next: next, Redactor: newTestRedactor(t)}
	want := errors.New("rate limited")

	tracer.Trace(context.Background(), "gemini", "пин-код 1234", "", want)

	if !errors.Is(next.err, want) {
		t.Errorf("err = %v, want %v", next.err, want)
	}
	if next.request != "пин-код ****" {
		t.Errorf("request = %q", next.request)
	}
}

// TestTracer_NilRedactorPassesThrough — redaction is optional, and the
// wrapper is installed unconditionally by cmd/miranda, so a nil Redactor must
// be a plain pass-through rather than a panic.
func TestTracer_NilRedactorPassesThrough(t *testing.T) {
	next := &recordingTracer{}
	tracer := &Tracer{Next: next}

	tracer.Trace(context.Background(), "anthropic", "пин-код 1234", "ок", nil)

	if next.request != "пин-код 1234" {
		t.Errorf("request = %q, want it unchanged", next.request)
	}
	if next.response != "ок" {
		t.Errorf("response = %q, want it unchanged", next.response)
	}
}

// TestTracer_NilNextDoesNotPanic — Next is required, but dropping the trace
// is a better failure than taking the process down over a log line.
func TestTracer_NilNextDoesNotPanic(t *testing.T) {
	tracer := &Tracer{Redactor: newTestRedactor(t)}
	tracer.Trace(context.Background(), "anthropic", "пин-код 1234", "ок", nil)

	var nilTracer *Tracer
	nilTracer.Trace(context.Background(), "anthropic", "пин-код 1234", "ок", nil)
}

package hub

import (
	"bytes"
	"strings"
	"sync"

	"github.com/archer-developer/miranda-llm/llmtrace/analyze"
)

// LLMTraceWriter returns an io.Writer publishing one Event per *completed*
// llmtrace block (see miranda-llm/llmtrace/analyze) to source, instead of
// one Event per raw line the way Writer does — a single traced call can
// span hundreds of lines (a long agent-loop conversation history piles up
// in the request), and the web UI's Logs screen only ever wants a whole
// call at a time, never a half-written one. Event.Data on the published
// event is an *analyze.Block, ready for the frontend to render directly
// with no client-side reassembly (see static/js/screens/logs.js).
func (h *Hub) LLMTraceWriter(source string) *LLMTraceWriter {
	return &LLMTraceWriter{hub: h, source: source, acc: analyze.NewAccumulator()}
}

// LLMTraceWriter is a line-buffering io.Writer adapter, same shape as Writer
// but feeding each line through an *analyze.Accumulator instead of
// publishing it verbatim — see Hub.LLMTraceWriter.
type LLMTraceWriter struct {
	hub    *Hub
	source string
	acc    *analyze.Accumulator

	mu  sync.Mutex
	buf bytes.Buffer
}

// Write implements io.Writer. Mirrors Writer.Write's own partial-line
// buffering (see that type's doc comment) — llmtrace.Logger.Trace's writes
// aren't guaranteed to land as whole lines either.
func (w *LLMTraceWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.buf.Write(p)
	for {
		line, err := w.buf.ReadString('\n')
		if err != nil {
			// No newline yet: put the partial line back and wait for more.
			w.buf.Reset()
			w.buf.WriteString(line)
			break
		}
		if block, ok := w.acc.Feed(strings.TrimSuffix(line, "\n")); ok {
			w.hub.Publish(Event{Source: w.source, Data: block})
		}
	}
	return len(p), nil
}

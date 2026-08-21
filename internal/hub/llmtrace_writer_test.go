package hub

import (
	"testing"

	"github.com/archer-developer/miranda-llm/llmtrace/analyze"
	"github.com/stretchr/testify/require"
)

func TestLLMTraceWriter_PublishesOneEventPerCompletedBlock(t *testing.T) {
	h := New(10)
	ch, _, unsubscribe := h.Subscribe(nil)
	defer unsubscribe()

	w := h.LLMTraceWriter("llm_log")
	trace := "=== 2026-08-21T10:00:00Z provider=gemini-agent conversation=session_1 ===\n" +
		"--- request ---\n" +
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}` + "\n" +
		"--- response ---\n" +
		`{"text":"hello","tool_calls":null}` + "\n" +
		"\n"

	_, err := w.Write([]byte(trace))
	require.NoError(t, err)

	ev := <-ch
	require.Equal(t, "llm_log", ev.Source)
	require.Empty(t, ev.Message, "the block goes in Data, not Message")

	block, ok := ev.Data.(*analyze.Block)
	require.True(t, ok, "Data must carry an *analyze.Block")
	require.Equal(t, "session_1", block.Conversation)
	require.Equal(t, "gemini-agent", block.Provider)
	require.Contains(t, block.Request, `"hi"`)
}

func TestLLMTraceWriter_NoEventUntilBlockCompletes(t *testing.T) {
	h := New(10)
	ch, _, unsubscribe := h.Subscribe(nil)
	defer unsubscribe()

	w := h.LLMTraceWriter("llm_log")
	_, err := w.Write([]byte("=== 2026-08-21T10:00:00Z provider=gemini-agent ===\n--- request ---\n{}\n"))
	require.NoError(t, err)
	require.Empty(t, ch, "no blank-line terminator yet: nothing published")

	_, err = w.Write([]byte("--- response ---\n{}\n\n"))
	require.NoError(t, err)
	require.NotEmpty(t, ch)
}

// TestLLMTraceWriter_BuffersPartialLineAcrossWrites mirrors
// TestWriter_BuffersPartialLineAcrossWrites — llmtrace.Logger.Trace's writes
// aren't guaranteed to land as whole lines any more here than for the plain
// Writer.
func TestLLMTraceWriter_BuffersPartialLineAcrossWrites(t *testing.T) {
	h := New(10)
	ch, _, unsubscribe := h.Subscribe(nil)
	defer unsubscribe()

	w := h.LLMTraceWriter("llm_log")
	_, err := w.Write([]byte("=== 2026-08-21T10:00:00Z provider=gemini-agent ===\n--- request ---\n{}\n--- resp"))
	require.NoError(t, err)
	require.Empty(t, ch)

	_, err = w.Write([]byte("onse ---\n{}\n\n"))
	require.NoError(t, err)
	require.NotEmpty(t, ch)
}

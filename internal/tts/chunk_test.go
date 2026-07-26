package tts

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestChunk_GroupsShortSentencesUntilForced is the regression test for the
// bug this greedy strategy fixes: cutting at the *first* sentence boundary
// seen split short sentences into their own chunk each, defeating a large
// maxChars entirely (observed in production logs: gemini_tts fired one API
// call per sentence despite chunk_max_chars: 200). Chunking must group all
// four into a single chunk instead, since together they still fit under
// maxChars.
func TestChunk_GroupsShortSentencesUntilForced(t *testing.T) {
	text := "Море — это стихия. Шум волн успокаивает разум. Люди веками стремились к морю. Оно хранит тайны глубин."
	chunks := Chunk(text, 200)
	require.Equal(t, []string{text}, chunks)
}

// TestChunk_CutsAtLatestBoundaryOnceForced verifies that once the buffer
// actually exceeds maxChars, the cut lands on the *latest* sentence boundary
// at or before the limit (grouping as much as still fits) rather than the
// earliest one, and leaves the remainder for the next chunk.
func TestChunk_CutsAtLatestBoundaryOnceForced(t *testing.T) {
	text := "Раз два три. Четыре пять шесть. Семь восемь девять. Десять одиннадцать двенадцать."
	chunks := Chunk(text, 40)
	require.Greater(t, len(chunks), 1)
	for _, c := range chunks {
		require.LessOrEqual(t, len([]rune(c)), 40)
	}
	require.Equal(t, text, joinWithSpace(chunks))
}

// TestChunk_HardSplitsLongSentenceAtWhitespace verifies a sentence is never
// split if a whitespace cut is available instead — only one giant run-on
// sentence with no boundary at all in range has to fall back to that.
func TestChunk_HardSplitsLongSentenceAtWhitespace(t *testing.T) {
	text := "это очень длинное предложение без единой точки которое обязательно превысит лимит символов и должно быть порезано по пробелам а не посередине слова"
	chunks := Chunk(text, 40)
	for _, c := range chunks {
		require.LessOrEqual(t, len([]rune(c)), 40)
		require.False(t, len(c) > 0 && c[0] == ' ')
	}
	require.Equal(t, text, joinWithSpace(chunks))
}

func TestChunk_RespectsMaxCharsLimit(t *testing.T) {
	chunks := Chunk("короткий ответ.", 100)
	require.Equal(t, []string{"короткий ответ."}, chunks)
}

// TestAccumulator_DoesNotEmitUntilForced verifies the accumulator doesn't
// flush a complete sentence just because one is available — it keeps
// buffering across multiple Push calls as long as the total still fits
// under maxChars, only emitting once forced or on Flush.
func TestAccumulator_DoesNotEmitUntilForced(t *testing.T) {
	a := NewAccumulator(200)

	require.Empty(t, a.Push("Море — это стихия."))
	require.Empty(t, a.Push(" Шум волн успокаивает разум."))

	final := a.Flush()
	require.Equal(t, []string{"Море — это стихия. Шум волн успокаивает разум."}, final)
}

func TestAccumulator_FlushOnEmptyBufferReturnsNothing(t *testing.T) {
	a := NewAccumulator(100)
	require.Empty(t, a.Flush())
}

func joinWithSpace(parts []string) string {
	out := parts[0]
	for _, p := range parts[1:] {
		out += " " + p
	}
	return out
}

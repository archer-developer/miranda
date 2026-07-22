package tts

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChunk_SplitsOnSentenceBoundaries(t *testing.T) {
	chunks := Chunk("Включил свет в зале. Также выключил телевизор.", 100)
	require.Equal(t, []string{"Включил свет в зале.", "Также выключил телевизор."}, chunks)
}

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

func TestAccumulator_EmitsChunkAsSoonAsSentenceCompletes(t *testing.T) {
	a := NewAccumulator(100)

	require.Empty(t, a.Push("Включил свет"))
	chunks := a.Push(" в зале. Ещё что")
	require.Equal(t, []string{"Включил свет в зале."}, chunks)

	final := a.Flush()
	require.Equal(t, []string{"Ещё что"}, final)
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

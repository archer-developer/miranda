// Package tts turns assistant text into speech via Yandex Station (the
// primary and only channel, dispatched through HA's
// media_player.play_media).
package tts

import "unicode"

// Accumulator buffers text as it streams in from the LLM and emits complete
// chunks — cut at a sentence boundary (.!?…) when possible, otherwise at the
// last whitespace before maxChars — so playback of the first sentence can
// start well before the model has finished generating the whole reply.
type Accumulator struct {
	maxChars int
	buf      []rune
}

// NewAccumulator creates an Accumulator that emits chunks no longer than
// maxChars.
func NewAccumulator(maxChars int) *Accumulator {
	if maxChars <= 0 {
		maxChars = 100
	}
	return &Accumulator{maxChars: maxChars}
}

// Push appends delta to the buffer and returns any chunks that are now
// complete.
func (a *Accumulator) Push(delta string) []string {
	a.buf = append(a.buf, []rune(delta)...)
	return a.drain()
}

// Flush returns any remaining buffered text as a final chunk (call once the
// underlying stream has ended) and resets the Accumulator.
func (a *Accumulator) Flush() []string {
	chunks := a.drain()
	if rest := trimSpace(a.buf); len(rest) > 0 {
		chunks = append(chunks, string(rest))
	}
	a.buf = nil
	return chunks
}

func (a *Accumulator) drain() []string {
	var chunks []string
	for {
		cut := nextChunkCut(a.buf, a.maxChars)
		if cut <= 0 {
			break
		}
		if chunk := trimSpace(a.buf[:cut]); len(chunk) > 0 {
			chunks = append(chunks, string(chunk))
		}
		a.buf = a.buf[cut:]
	}
	return chunks
}

// Chunk splits a complete, already-final text into pieces no longer than
// maxChars in one call, for callers that aren't streaming.
func Chunk(text string, maxChars int) []string {
	a := NewAccumulator(maxChars)
	chunks := a.Push(text)
	return append(chunks, a.Flush()...)
}

// nextChunkCut returns the index (exclusive) to cut buf at for one complete
// chunk, or 0 if buf doesn't yet contain a full chunk worth emitting.
func nextChunkCut(buf []rune, maxChars int) int {
	if len(buf) == 0 {
		return 0
	}

	// Prefer the earliest sentence-ending punctuation (followed by
	// whitespace or end of buffer) that still fits within maxChars.
	for i, r := range buf {
		if i+1 > maxChars {
			break
		}
		if isSentenceEnd(r) && (i+1 == len(buf) || unicode.IsSpace(buf[i+1])) {
			return i + 1
		}
	}

	if len(buf) <= maxChars {
		return 0 // no sentence boundary yet, and not forced to cut yet
	}

	// Buffer exceeds the limit with no sentence boundary in range: cut at
	// the last whitespace at or before maxChars to avoid splitting a word.
	for i := maxChars; i > 0; i-- {
		if unicode.IsSpace(buf[i-1]) {
			return i
		}
	}
	// No whitespace at all (one giant token) — hard-cut at the limit.
	return maxChars
}

func isSentenceEnd(r rune) bool {
	return r == '.' || r == '!' || r == '?' || r == '…'
}

func trimSpace(runes []rune) []rune {
	start := 0
	for start < len(runes) && unicode.IsSpace(runes[start]) {
		start++
	}
	end := len(runes)
	for end > start && unicode.IsSpace(runes[end-1]) {
		end--
	}
	return runes[start:end]
}

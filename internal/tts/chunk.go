// Package tts turns assistant text into speech via Yandex Station (the
// always-available provider, dispatched through HA's media_player.play_media)
// or, optionally, Gemini (an external voice rendered to a file and played
// back through the same station — see gemini.go).
package tts

import "unicode"

// Accumulator buffers text as it streams in from the LLM and emits complete
// chunks once one is ready. Chunking is always maximally greedy: it never
// cuts just because a sentence boundary is available — it keeps buffering
// until forced past maxChars, then cuts at the *latest* sentence boundary
// (.!?…) at or before maxChars, so a chunk packs in as many complete
// sentences as fit rather than firing one request/utterance per sentence.
// Only if the buffer exceeds maxChars with no sentence boundary in range at
// all (one very long run-on sentence) does the cut fall back to the last
// whitespace at or before maxChars, or a hard cut as a last resort — a
// sentence is never split if a whitespace-or-boundary cut is available.
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
// chunk, or 0 if buf isn't yet forced to cut (still under maxChars) — see
// Accumulator's doc comment for the greedy strategy this implements.
func nextChunkCut(buf []rune, maxChars int) int {
	if len(buf) == 0 || len(buf) <= maxChars {
		return 0
	}
	if i := latestSentenceEnd(buf, maxChars); i > 0 {
		return i
	}
	return forceCut(buf, maxChars)
}

// latestSentenceEnd returns the index just past the last sentence-ending
// punctuation (followed by whitespace or end of buffer) at or before
// maxChars, or 0 if there is none.
func latestSentenceEnd(buf []rune, maxChars int) int {
	last := 0
	for i, r := range buf {
		if i+1 > maxChars {
			break
		}
		if isSentenceEnd(r) && (i+1 == len(buf) || unicode.IsSpace(buf[i+1])) {
			last = i + 1
		}
	}
	return last
}

// forceCut cuts buf at the last whitespace at or before maxChars, to avoid
// splitting a word, or hard-cuts at maxChars if there's no whitespace at all
// (one giant token).
func forceCut(buf []rune, maxChars int) int {
	for i := maxChars; i > 0; i-- {
		if unicode.IsSpace(buf[i-1]) {
			return i
		}
	}
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

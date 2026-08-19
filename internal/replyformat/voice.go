package replyformat

import (
	"strings"
	"unicode/utf8"
)

// ToVoiceText renders blocks as plain text safe to feed straight into TTS:
// every markup delimiter is dropped (bold/italic/code segments contribute
// only their bare text), a link contributes only its label — its URL is
// never spoken — and list items/paragraphs are joined into one
// space-separated stream, each normalized to end in sentence punctuation
// first so a TTS engine reads them as separate sentences rather than
// running them together. Not a hardcoded Russian conjunction or any other
// language-specific joiner: the model can reply in any language, so
// joining is purely punctuation-based.
func ToVoiceText(blocks []Block) string {
	var parts []string
	for _, blk := range blocks {
		switch blk.Type {
		case BlockList:
			for _, item := range blk.Items {
				if s := plainSegments(item); s != "" {
					parts = append(parts, normalizeSentence(s))
				}
			}
		default:
			if s := plainSegments(blk.Segments); s != "" {
				parts = append(parts, normalizeSentence(s))
			}
		}
	}
	return strings.Join(parts, " ")
}

// plainSegments concatenates every segment's Text, discarding type and URL
// — the one place a link's URL is dropped rather than carried through.
func plainSegments(segs []Segment) string {
	var b strings.Builder
	for _, seg := range segs {
		b.WriteString(seg.Text)
	}
	return strings.TrimSpace(b.String())
}

// sentenceEndPunctuation is what normalizeSentence treats as already
// providing the pause a TTS engine needs before the next block/item — a
// period is only appended when none of these are already present.
const sentenceEndPunctuation = ".!?:;,-—"

func normalizeSentence(s string) string {
	last, _ := utf8.DecodeLastRuneInString(s)
	if strings.ContainsRune(sentenceEndPunctuation, last) {
		return s
	}
	return s + "."
}

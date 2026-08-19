package replyformat

import (
	"strings"
	"unicode/utf8"
)

// ToTelegramHTMLChunks renders blocks as Telegram Bot API HTML
// (parse_mode=HTML), split into chunks of at most maxChars runes each.
// Telegram's sendMessage hard-rejects the whole call if its HTML is
// malformed, so a chunk boundary can never fall inside a tag — only
// between complete words. When rendering one block's content would alone
// exceed maxChars, the split falls back to word boundaries within that
// block, closing whatever tag is open at the forced cut and reopening it
// at the start of the next chunk, so every returned chunk is
// independently well-formed HTML.
//
// List items render as "• item" lines — Telegram's HTML subset has no
// <ul>/<li>.
func ToTelegramHTMLChunks(blocks []Block, maxChars int) []string {
	b := &telegramBuilder{maxChars: maxChars}
	for _, tok := range tokenizeTelegram(blocks) {
		b.addToken(tok)
	}
	b.closeOpenTag()
	b.flush()
	return b.chunks
}

// telegramToken is one renderable word plus the separator (if any) that
// should precede it — a blank chunk never starts with a leftover
// separator, since sep is only written when appending to a non-empty
// chunk (see telegramBuilder.addToken).
type telegramToken struct {
	sep  string // "", " ", "\n", or "\n\n"
	text string
	seg  SegmentType
	url  string
}

// tokenizeTelegram flattens blocks into a word stream: blocks are
// separated by a blank line, list items by a single newline plus a "•"
// bullet word, and words within one segment's text by single spaces
// (strings.Fields collapses whatever whitespace the model wrote — fine
// for a chat rendering, where original line-wrapping isn't meaningful).
func tokenizeTelegram(blocks []Block) []telegramToken {
	var toks []telegramToken
	for bi, blk := range blocks {
		sep := ""
		if bi > 0 {
			sep = "\n\n"
		}
		switch blk.Type {
		case BlockList:
			for ii, item := range blk.Items {
				itemSep := sep
				if ii > 0 {
					itemSep = "\n"
				}
				toks = append(toks, telegramToken{sep: itemSep, text: "•", seg: SegmentText})
				toks = append(toks, segmentTokens(item, " ")...)
				sep = ""
			}
		default:
			toks = append(toks, segmentTokens(blk.Segments, sep)...)
		}
	}
	return toks
}

func segmentTokens(segs []Segment, firstSep string) []telegramToken {
	var toks []telegramToken
	sep := firstSep
	for _, seg := range segs {
		for _, w := range strings.Fields(seg.Text) {
			toks = append(toks, telegramToken{sep: sep, text: w, seg: seg.Type, url: seg.URL})
			sep = " "
		}
	}
	return toks
}

// telegramBuilder accumulates tokens into HTML chunks. Only one tag can be
// open at a time (openTag/openURL) — replyformat.Segment has no nesting,
// so this never needs a tag stack.
type telegramBuilder struct {
	maxChars int
	chunks   []string
	cur      strings.Builder
	curLen   int
	openTag  SegmentType // "" means no tag currently open
	openURL  string
}

// addToken appends one token to the current chunk, forcing a new chunk
// first if the token (plus a worst-case tag transition) wouldn't fit. The
// overflow check always budgets for closing whatever tag is currently open
// even when it may turn out to stay open — a deliberately conservative
// overestimate that keeps the arithmetic simple and can only under-fill a
// chunk, never split one mid-tag.
func (b *telegramBuilder) addToken(tok telegramToken) {
	key, keyURL := tagKey(tok.seg, tok.url)
	escaped := escapeHTML(tok.text)
	switching := key != b.openTag || keyURL != b.openURL
	needed := utf8.RuneCountInString(tok.sep) + utf8.RuneCountInString(escaped) + tagOverhead(key, keyURL)
	if switching {
		needed += tagOverhead(b.openTag, b.openURL)
	}

	if b.curLen > 0 && b.curLen+needed > b.maxChars {
		// Forced cut: close whatever tag is open, start a fresh chunk, and
		// drop the pending separator — a chunk never opens with a blank
		// line/space. Re-derive switching against the now-reset state so a
		// tag that was open across the cut gets reopened below.
		b.closeOpenTag()
		b.flush()
		switching = key != b.openTag || keyURL != b.openURL
	} else {
		// Close before writing the separator, not after — otherwise the
		// separator ends up inside the closing tag's span
		// ("bold </b>text" instead of "bold</b> text").
		if switching {
			b.closeOpenTag()
		}
		if b.curLen > 0 {
			b.writeDirect(tok.sep)
		}
	}

	if switching {
		b.openTagFor(key, keyURL)
	}
	b.writeDirect(escaped)
}

func (b *telegramBuilder) writeDirect(s string) {
	if s == "" {
		return
	}
	b.cur.WriteString(s)
	b.curLen += utf8.RuneCountInString(s)
}

func (b *telegramBuilder) closeOpenTag() {
	if b.openTag == "" {
		return
	}
	b.writeDirect(closingTag(b.openTag))
	b.openTag = ""
	b.openURL = ""
}

func (b *telegramBuilder) openTagFor(key SegmentType, url string) {
	b.writeDirect(openingTag(key, url))
	b.openTag = key
	b.openURL = url
}

func (b *telegramBuilder) flush() {
	if b.cur.Len() == 0 {
		return
	}
	b.chunks = append(b.chunks, b.cur.String())
	b.cur.Reset()
	b.curLen = 0
}

// tagKey normalizes a token's segment into the (type, url) pair used for
// open-tag bookkeeping: SegmentText (and the zero value) both mean "no tag
// open", so they compare equal to each other regardless of url.
func tagKey(seg SegmentType, url string) (SegmentType, string) {
	if seg == SegmentText || seg == "" {
		return "", ""
	}
	return seg, url
}

func closingTag(t SegmentType) string {
	switch t {
	case SegmentBold:
		return "</b>"
	case SegmentItalic:
		return "</i>"
	case SegmentCode:
		return "</code>"
	case SegmentLink:
		return "</a>"
	default:
		return ""
	}
}

func openingTag(t SegmentType, url string) string {
	switch t {
	case SegmentBold:
		return "<b>"
	case SegmentItalic:
		return "<i>"
	case SegmentCode:
		return "<code>"
	case SegmentLink:
		return `<a href="` + escapeAttr(url) + `">`
	default:
		return ""
	}
}

func tagOverhead(t SegmentType, url string) int {
	return utf8.RuneCountInString(openingTag(t, url)) + utf8.RuneCountInString(closingTag(t))
}

// escapeHTML escapes the three bytes Telegram's HTML parse_mode requires
// escaped in text content: "&" first, so it doesn't double-escape the
// entities produced for "<"/">".
func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// escapeAttr is escapeHTML plus '"', for a value placed inside a
// double-quoted HTML attribute (href="...").
func escapeAttr(s string) string {
	return strings.ReplaceAll(escapeHTML(s), `"`, "&quot;")
}

// Package replyformat owns the one canonical parse of a reply's
// markdown-lite text (bold/italic/code/links/simple lists — never full
// CommonMark) into a shared AST, plus per-channel renderers off that same
// AST (see telegram.go, voice.go; the web UI consumes Block/Segment
// directly as JSON — see their struct tags).
//
// history.Message.Content is never touched by any of this — it stays
// exactly what the model said, forever, the same principle
// history.DownloadRef established for file references: structured,
// derived data is carried alongside the raw text, never folded into it.
package replyformat

import (
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// BlockType distinguishes the two block-level constructs Parse produces.
type BlockType string

// The two BlockType values Parse produces.
const (
	BlockParagraph BlockType = "paragraph"
	BlockList      BlockType = "list"
)

// Block is one paragraph or list parsed out of a reply. Segments is set for
// BlockParagraph; Items (one Segment run per list item) is set for
// BlockList — never both, mirroring the comment on the type itself.
type Block struct {
	Type     BlockType   `json:"type"`
	Segments []Segment   `json:"segments,omitempty"`
	Items    [][]Segment `json:"items,omitempty"`
}

// SegmentType distinguishes the inline constructs Parse recognizes within a
// paragraph or list item. There is deliberately no nested emphasis in v1 —
// a segment is one flat type, not a style bitmask — matching the existing
// minimal-markdown precedent (internal/webui/static/js/inline-text.js);
// additive to extend later if nesting turns out to matter.
type SegmentType string

// The SegmentType values Parse recognizes.
const (
	SegmentText   SegmentType = "text"
	SegmentBold   SegmentType = "bold"
	SegmentItalic SegmentType = "italic"
	SegmentCode   SegmentType = "code"
	SegmentLink   SegmentType = "link"
)

// Segment is one run of inline text and how it should be presented. URL is
// set only for SegmentLink.
type Segment struct {
	Type SegmentType `json:"type"`
	Text string      `json:"text"`
	URL  string      `json:"url,omitempty"`
}

// listItemRE matches one markdown-lite list item line: "-" or "*", exactly
// one space, then non-space content. The mandatory marker+space is what
// disambiguates a list item from italic: "- text" is a list item, "*text*"
// (no space after the marker) is italic — lexically distinct, the same
// rule CommonMark uses for the same reason.
var listItemRE = regexp.MustCompile(`^[-*] (\S.*)$`)

// Parse splits text into blocks and parses each block's inline markup. It
// never errors: any construct that doesn't parse as intended (an unmatched
// "*", a link missing its "(url)") degrades to literal text rather than
// being dropped or causing a failure. Returns nil for empty/whitespace-only
// input, otherwise at least one block.
func Parse(text string) []Block {
	normalized := strings.ReplaceAll(text, "\r\n", "\n")
	var blocks []Block
	for _, chunk := range splitChunks(normalized) {
		blocks = append(blocks, parseChunk(chunk)...)
	}
	if blocks == nil && strings.TrimSpace(text) != "" {
		// Degenerate input (e.g. only control characters) that somehow
		// produced no chunks — fall back to one literal paragraph rather
		// than silently dropping the reply.
		blocks = []Block{{Type: BlockParagraph, Segments: []Segment{{Type: SegmentText, Text: text}}}}
	}
	return blocks
}

// splitChunks groups text's lines into paragraphs-worth of consecutive
// non-blank lines, the way blank lines delimit paragraphs in every
// markdown dialect. Each returned chunk still has its internal newlines —
// parseChunk is what further splits a chunk into paragraph/list blocks.
func splitChunks(text string) []string {
	var chunks []string
	var cur []string
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			if len(cur) > 0 {
				chunks = append(chunks, strings.Join(cur, "\n"))
				cur = nil
			}
			continue
		}
		cur = append(cur, line)
	}
	if len(cur) > 0 {
		chunks = append(chunks, strings.Join(cur, "\n"))
	}
	return chunks
}

// parseChunk splits one blank-line-delimited chunk into paragraph/list
// blocks: consecutive list-item lines become one list block, everything
// else one paragraph block, in the order encountered — so a single chunk
// like "Вот варианты:\n- Раз\n- Два" yields [paragraph, list], matching how
// the model naturally writes an intro line followed by options.
func parseChunk(chunk string) []Block {
	var blocks []Block
	var paraLines []string
	var listItems [][]Segment

	flushPara := func() {
		if len(paraLines) > 0 {
			blocks = append(blocks, Block{Type: BlockParagraph, Segments: parseInline(strings.Join(paraLines, "\n"))})
			paraLines = nil
		}
	}
	flushList := func() {
		if len(listItems) > 0 {
			blocks = append(blocks, Block{Type: BlockList, Items: listItems})
			listItems = nil
		}
	}

	for _, line := range strings.Split(chunk, "\n") {
		if m := listItemRE.FindStringSubmatch(line); m != nil {
			flushPara()
			listItems = append(listItems, parseInline(m[1]))
			continue
		}
		flushList()
		paraLines = append(paraLines, line)
	}
	flushPara()
	flushList()
	return blocks
}

// parseInline scans one paragraph's or list item's text for inline
// constructs, in priority order at each position: code span (highest —
// identical semantics to the existing JS CODE_SPAN_RE), link, bold,
// italic. Byte-level scanning for the ASCII delimiters is safe against
// multi-byte UTF-8 text (e.g. Russian replies): every marker byte is < 0x80,
// and UTF-8 continuation bytes are always >= 0x80, so a marker byte can
// never be mistaken for part of a multi-byte rune.
func parseInline(text string) []Segment {
	var segs []Segment
	var buf strings.Builder
	flush := func() {
		if buf.Len() > 0 {
			segs = append(segs, Segment{Type: SegmentText, Text: buf.String()})
			buf.Reset()
		}
	}

	i := 0
	for i < len(text) {
		switch text[i] {
		case '`':
			if j := strings.IndexByte(text[i+1:], '`'); j >= 0 {
				inner := text[i+1 : i+1+j]
				if inner != "" && !strings.Contains(inner, "\n") {
					flush()
					segs = append(segs, Segment{Type: SegmentCode, Text: inner})
					i = i + 1 + j + 1
					continue
				}
			}
		case '[':
			if end, ok := parseLink(text, i); ok {
				flush()
				segs = append(segs, end.seg)
				i = end.next
				continue
			}
		case '*':
			if strings.HasPrefix(text[i:], "**") {
				if j := strings.Index(text[i+2:], "**"); j >= 0 {
					inner := text[i+2 : i+2+j]
					if inner != "" {
						flush()
						segs = append(segs, Segment{Type: SegmentBold, Text: inner})
						i = i + 2 + j + 2
						continue
					}
				}
			} else if j := strings.IndexByte(text[i+1:], '*'); j >= 0 {
				inner := text[i+1 : i+1+j]
				if inner != "" {
					flush()
					segs = append(segs, Segment{Type: SegmentItalic, Text: inner})
					i = i + 1 + j + 1
					continue
				}
			}
		case '_':
			if next, ok := parseUnderscoreItalic(text, i); ok {
				flush()
				segs = append(segs, Segment{Type: SegmentItalic, Text: text[i+1 : next-1]})
				i = next
				continue
			}
		}
		_, size := utf8.DecodeRuneInString(text[i:])
		buf.WriteString(text[i : i+size])
		i += size
	}
	flush()
	return segs
}

type linkMatch struct {
	seg  Segment
	next int
}

// parseLink attempts to match "[label](url)" starting at text[i] (text[i]
// == '['). url may not contain whitespace — matches how a bare URL is
// written in practice and keeps the closing ")" unambiguous. A url whose
// scheme isn't in isSafeLinkScheme's allowlist is rejected outright (the
// whole "[label](url)" degrades to literal text, same as any other
// unmatched/malformed construct) rather than ever becoming a SegmentLink —
// see that function's doc comment for why.
func parseLink(text string, i int) (linkMatch, bool) {
	closeLabel := strings.IndexByte(text[i+1:], ']')
	if closeLabel < 0 {
		return linkMatch{}, false
	}
	labelEnd := i + 1 + closeLabel
	label := text[i+1 : labelEnd]
	if label == "" || labelEnd+1 >= len(text) || text[labelEnd+1] != '(' {
		return linkMatch{}, false
	}
	closeURL := strings.IndexByte(text[labelEnd+2:], ')')
	if closeURL < 0 {
		return linkMatch{}, false
	}
	urlEnd := labelEnd + 2 + closeURL
	rawURL := text[labelEnd+2 : urlEnd]
	if rawURL == "" || strings.ContainsAny(rawURL, " \t\n") || !isSafeLinkScheme(rawURL) {
		return linkMatch{}, false
	}
	return linkMatch{seg: Segment{Type: SegmentLink, Text: label, URL: rawURL}, next: urlEnd + 1}, true
}

// isSafeLinkScheme reports whether rawURL is safe to ever place in a real,
// clickable href — every renderer off this package's Block/Segment AST
// (the web UI's segment-template.js, ToTelegramHTMLChunks) trusts a
// SegmentLink's URL by construction, so the check has to happen once here
// rather than being re-implemented (and possibly forgotten) per renderer.
// Not a hypothetical: Segment.URL is arbitrary model output, and a page
// fetched via the web_fetch/MCP tools could contain attacker-controlled
// markdown-link syntax that ends up echoed back into the model's own reply
// text (indirect prompt injection) — "javascript:"/"data:"/"vbscript:" in
// an href is live code in the web UI's authenticated origin, not inert
// text like everything else in a reply. Empty scheme (a relative URL, e.g.
// "/api/files/abc") is allowed since it can only ever resolve to http(s)
// of whatever origin renders it.
func isSafeLinkScheme(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "", "http", "https", "mailto":
		return true
	default:
		return false
	}
}

// parseUnderscoreItalic attempts to match "_text_" starting at text[i]
// (text[i] == '_'), requiring a word boundary immediately outside both
// delimiters — the rune before the opening "_" and the rune after the
// closing "_" must not themselves be letters/digits/underscore — so
// "snake_case_var" is never mis-italicized. Returns the index just past
// the closing "_" on success.
func parseUnderscoreItalic(text string, i int) (int, bool) {
	if r, ok := precedingRune(text, i); ok && isWordRune(r) {
		return 0, false
	}
	j := strings.IndexByte(text[i+1:], '_')
	if j < 0 {
		return 0, false
	}
	inner := text[i+1 : i+1+j]
	next := i + 1 + j + 1
	if inner == "" || strings.Contains(inner, "\n") {
		return 0, false
	}
	if r, ok := runeAt(text, next); ok && isWordRune(r) {
		return 0, false
	}
	return next, true
}

func isWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

func precedingRune(text string, i int) (rune, bool) {
	if i == 0 {
		return 0, false
	}
	r, _ := utf8.DecodeLastRuneInString(text[:i])
	return r, true
}

func runeAt(text string, i int) (rune, bool) {
	if i >= len(text) {
		return 0, false
	}
	r, _ := utf8.DecodeRuneInString(text[i:])
	return r, true
}

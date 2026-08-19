package replyformat

import (
	"regexp"
	"strings"
	"testing"
)

func TestToTelegramHTMLChunks_PerConstruct(t *testing.T) {
	tests := []struct {
		name string
		in   []Block
		want string
	}{
		{
			name: "plain text",
			in:   Parse("hello world"),
			want: "hello world",
		},
		{
			name: "bold",
			in:   Parse("this is **bold** text"),
			want: "this is <b>bold</b> text",
		},
		{
			name: "italic",
			in:   Parse("this is *italic* text"),
			want: "this is <i>italic</i> text",
		},
		{
			name: "code",
			in:   Parse("run `go test` now"),
			want: "run <code>go test</code> now",
		},
		{
			name: "link",
			in:   Parse("see [docs](https://example.com) now"),
			want: `see <a href="https://example.com">docs</a> now`,
		},
		{
			name: "list",
			in:   Parse("- one\n- two"),
			want: "• one\n• two",
		},
		{
			name: "escaping in plain text",
			in:   Parse("a < b & c > d"),
			want: "a &lt; b &amp; c &gt; d",
		},
		{
			name: "escaping inside bold",
			in:   Parse("**a < b & c**"),
			want: "<b>a &lt; b &amp; c</b>",
		},
		{
			name: "two paragraphs",
			in:   Parse("first\n\nsecond"),
			want: "first\n\nsecond",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chunks := ToTelegramHTMLChunks(tt.in, 4000)
			if len(chunks) != 1 {
				t.Fatalf("ToTelegramHTMLChunks() = %d chunks, want 1: %#v", len(chunks), chunks)
			}
			if chunks[0] != tt.want {
				t.Errorf("ToTelegramHTMLChunks() = %q, want %q", chunks[0], tt.want)
			}
		})
	}
}

// TestToTelegramHTMLChunks_BoldSpanStraddlesChunkBoundary is the dedicated
// test the plan calls out as the trickiest correctness requirement: a bold
// span long enough that a small maxChars forces the split to land inside
// it. Every returned chunk must be independently well-formed HTML (a
// dangling "<b>" would make Telegram reject the whole sendMessage call),
// and the original words must all survive, in order, once tags are
// stripped.
func TestToTelegramHTMLChunks_BoldSpanStraddlesChunkBoundary(t *testing.T) {
	blocks := []Block{{
		Type: BlockParagraph,
		Segments: []Segment{
			{Type: SegmentBold, Text: "one two three four five six seven eight nine ten"},
		},
	}}

	chunks := ToTelegramHTMLChunks(blocks, 20)
	if len(chunks) < 2 {
		t.Fatalf("expected the bold span to be split across multiple chunks, got %d: %#v", len(chunks), chunks)
	}

	var words []string
	for _, chunk := range chunks {
		if strings.Count(chunk, "<b>") != strings.Count(chunk, "</b>") {
			t.Errorf("chunk has unbalanced <b> tags: %q", chunk)
		}
		if idx := strings.Index(chunk, "</b>"); idx >= 0 && strings.Index(chunk, "<b>") > idx {
			t.Errorf("chunk closes </b> before opening <b>: %q", chunk)
		}
		stripped := regexp.MustCompile(`</?b>`).ReplaceAllString(chunk, "")
		words = append(words, strings.Fields(stripped)...)
	}

	got := strings.Join(words, " ")
	want := "one two three four five six seven eight nine ten"
	if got != want {
		t.Errorf("reconstructed words = %q, want %q", got, want)
	}
}

// TestToTelegramHTMLChunks_LinkSpanStraddlesChunkBoundary mirrors the bold
// test for a link, which additionally carries an href attribute that must
// survive being reopened on the far side of a forced cut.
func TestToTelegramHTMLChunks_LinkSpanStraddlesChunkBoundary(t *testing.T) {
	blocks := []Block{{
		Type: BlockParagraph,
		Segments: []Segment{
			{Type: SegmentLink, Text: "one two three four five six seven eight", URL: "https://example.com/path"},
		},
	}}

	chunks := ToTelegramHTMLChunks(blocks, 20)
	if len(chunks) < 2 {
		t.Fatalf("expected the link span to be split across multiple chunks, got %d: %#v", len(chunks), chunks)
	}

	linkOpenRE := regexp.MustCompile(`<a href="https://example\.com/path">`)
	for _, chunk := range chunks {
		opens := len(linkOpenRE.FindAllString(chunk, -1))
		closes := strings.Count(chunk, "</a>")
		if opens != closes {
			t.Errorf("chunk has unbalanced <a> tags: %q", chunk)
		}
		// Any <a...> in a chunk must use the same href every time it
		// reopens — reopening across a forced cut must not lose/garble it.
		for _, m := range regexp.MustCompile(`<a href="([^"]*)">`).FindAllStringSubmatch(chunk, -1) {
			if m[1] != "https://example.com/path" {
				t.Errorf("chunk reopened <a> with wrong href %q: %q", m[1], chunk)
			}
		}
	}
}

func TestToTelegramHTMLChunks_Empty(t *testing.T) {
	if got := ToTelegramHTMLChunks(nil, 100); len(got) != 0 {
		t.Errorf("ToTelegramHTMLChunks(nil, 100) = %#v, want empty", got)
	}
}

package replyformat

import (
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []Block
	}{
		{
			name: "plain text",
			in:   "hello world",
			want: []Block{
				{Type: BlockParagraph, Segments: []Segment{{Type: SegmentText, Text: "hello world"}}},
			},
		},
		{
			name: "bold",
			in:   "this is **bold** text",
			want: []Block{
				{Type: BlockParagraph, Segments: []Segment{
					{Type: SegmentText, Text: "this is "},
					{Type: SegmentBold, Text: "bold"},
					{Type: SegmentText, Text: " text"},
				}},
			},
		},
		{
			name: "italic star",
			in:   "this is *italic* text",
			want: []Block{
				{Type: BlockParagraph, Segments: []Segment{
					{Type: SegmentText, Text: "this is "},
					{Type: SegmentItalic, Text: "italic"},
					{Type: SegmentText, Text: " text"},
				}},
			},
		},
		{
			name: "italic underscore",
			in:   "this is _italic_ text",
			want: []Block{
				{Type: BlockParagraph, Segments: []Segment{
					{Type: SegmentText, Text: "this is "},
					{Type: SegmentItalic, Text: "italic"},
					{Type: SegmentText, Text: " text"},
				}},
			},
		},
		{
			name: "underscore inside identifier is not italic",
			in:   "check snake_case_var here",
			want: []Block{
				{Type: BlockParagraph, Segments: []Segment{{Type: SegmentText, Text: "check snake_case_var here"}}},
			},
		},
		{
			name: "code span highest priority",
			in:   "run `go test ./...` now",
			want: []Block{
				{Type: BlockParagraph, Segments: []Segment{
					{Type: SegmentText, Text: "run "},
					{Type: SegmentCode, Text: "go test ./..."},
					{Type: SegmentText, Text: " now"},
				}},
			},
		},
		{
			name: "code span suppresses markup inside it",
			in:   "use `**not bold**` here",
			want: []Block{
				{Type: BlockParagraph, Segments: []Segment{
					{Type: SegmentText, Text: "use "},
					{Type: SegmentCode, Text: "**not bold**"},
					{Type: SegmentText, Text: " here"},
				}},
			},
		},
		{
			name: "link",
			in:   "see [the docs](https://example.com/docs) for more",
			want: []Block{
				{Type: BlockParagraph, Segments: []Segment{
					{Type: SegmentText, Text: "see "},
					{Type: SegmentLink, Text: "the docs", URL: "https://example.com/docs"},
					{Type: SegmentText, Text: " for more"},
				}},
			},
		},
		{
			name: "list vs italic disambiguation",
			in:   "- one\n- two",
			want: []Block{
				{Type: BlockList, Items: [][]Segment{
					{{Type: SegmentText, Text: "one"}},
					{{Type: SegmentText, Text: "two"}},
				}},
			},
		},
		{
			name: "star with no following space is italic not a list item",
			in:   "*text* not a list",
			want: []Block{
				{Type: BlockParagraph, Segments: []Segment{
					{Type: SegmentItalic, Text: "text"},
					{Type: SegmentText, Text: " not a list"},
				}},
			},
		},
		{
			name: "mixed paragraph and list in one chunk",
			in:   "Вот варианты:\n- Раз\n- Два",
			want: []Block{
				{Type: BlockParagraph, Segments: []Segment{{Type: SegmentText, Text: "Вот варианты:"}}},
				{Type: BlockList, Items: [][]Segment{
					{{Type: SegmentText, Text: "Раз"}},
					{{Type: SegmentText, Text: "Два"}},
				}},
			},
		},
		{
			name: "blank line separates paragraphs",
			in:   "first paragraph\n\nsecond paragraph",
			want: []Block{
				{Type: BlockParagraph, Segments: []Segment{{Type: SegmentText, Text: "first paragraph"}}},
				{Type: BlockParagraph, Segments: []Segment{{Type: SegmentText, Text: "second paragraph"}}},
			},
		},
		{
			name: "unmatched opener degrades to literal text",
			in:   "unterminated *italic here",
			want: []Block{
				{Type: BlockParagraph, Segments: []Segment{{Type: SegmentText, Text: "unterminated *italic here"}}},
			},
		},
		{
			name: "unmatched link brackets degrade to literal text",
			in:   "see [broken link here",
			want: []Block{
				{Type: BlockParagraph, Segments: []Segment{{Type: SegmentText, Text: "see [broken link here"}}},
			},
		},
		{
			name: "javascript scheme link degrades to literal text",
			in:   "click [here](javascript:alert(1))",
			want: []Block{
				{Type: BlockParagraph, Segments: []Segment{{Type: SegmentText, Text: "click [here](javascript:alert(1))"}}},
			},
		},
		{
			name: "data scheme link degrades to literal text",
			in:   "see [this](data:text/html,evil)",
			want: []Block{
				{Type: BlockParagraph, Segments: []Segment{{Type: SegmentText, Text: "see [this](data:text/html,evil)"}}},
			},
		},
		{
			name: "mailto link is allowed",
			in:   "email [me](mailto:a@b.com)",
			want: []Block{
				{Type: BlockParagraph, Segments: []Segment{
					{Type: SegmentText, Text: "email "},
					{Type: SegmentLink, Text: "me", URL: "mailto:a@b.com"},
				}},
			},
		},
		{
			name: "relative link is allowed",
			in:   "see [this file](/api/files/abc)",
			want: []Block{
				{Type: BlockParagraph, Segments: []Segment{
					{Type: SegmentText, Text: "see "},
					{Type: SegmentLink, Text: "this file", URL: "/api/files/abc"},
				}},
			},
		},
		{
			name: "unmatched code backtick degrades to literal text",
			in:   "a stray ` backtick",
			want: []Block{
				{Type: BlockParagraph, Segments: []Segment{{Type: SegmentText, Text: "a stray ` backtick"}}},
			},
		},
		{
			name: "empty input",
			in:   "",
			want: nil,
		},
		{
			name: "whitespace-only input",
			in:   "   \n\n  ",
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Parse(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Parse(%q) =\n  %#v\nwant\n  %#v", tt.in, got, tt.want)
			}
		})
	}
}

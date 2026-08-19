package replyformat

import (
	"strings"
	"testing"
)

func TestToVoiceText(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "plain text unchanged",
			in:   "hello world",
			want: "hello world.",
		},
		{
			name: "bold delimiters dropped",
			in:   "this is **bold** text",
			want: "this is bold text.",
		},
		{
			name: "italic delimiters dropped",
			in:   "this is *italic* text and _also this_",
			want: "this is italic text and also this.",
		},
		{
			name: "code delimiters dropped",
			in:   "run `go test` now",
			want: "run go test now.",
		},
		{
			name: "link keeps label drops url",
			in:   "see [the docs](https://example.com/secret) for more",
			want: "see the docs for more.",
		},
		{
			name: "existing terminal punctuation is not duplicated",
			in:   "already done!",
			want: "already done!",
		},
		{
			name: "list items become separate punctuated sentences",
			in:   "Вот варианты:\n- Раз\n- Два",
			want: "Вот варианты: Раз. Два.",
		},
		{
			name: "multiple paragraphs are space joined",
			in:   "first paragraph\n\nsecond paragraph",
			want: "first paragraph. second paragraph.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ToVoiceText(Parse(tt.in))
			if got != tt.want {
				t.Errorf("ToVoiceText(Parse(%q)) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestToVoiceText_URLNeverAppearsInOutput(t *testing.T) {
	blocks := Parse("click [here](https://example.com/very-secret-path?token=abc123) now")
	got := ToVoiceText(blocks)
	if strings.Contains(got, "example.com") || strings.Contains(got, "https") {
		t.Errorf("ToVoiceText() leaked the URL into spoken text: %q", got)
	}
}

func TestToVoiceText_Empty(t *testing.T) {
	if got := ToVoiceText(nil); got != "" {
		t.Errorf("ToVoiceText(nil) = %q, want empty", got)
	}
}

package gmail

import (
	"testing"
	"time"
)

func TestStripHTML(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "plain text passthrough",
			input: "hello world",
			want:  "hello world",
		},
		{
			name:  "simple tags removed",
			input: "<p>hello</p> <b>world</b>",
			want:  "hello\n\nworld",
		},
		{
			name:  "br tags become newlines",
			input: "line one<br>line two<br/>line three<br />line four",
			want:  "line one\nline two\nline three\nline four",
		},
		{
			name:  "entities decoded",
			input: "Tom &amp; Jerry &lt;3 &gt; &quot;friends&quot;",
			want:  "Tom & Jerry <3 > \"friends\"",
		},
		{
			name:  "nbsp decoded",
			input: "spaced&nbsp;out",
			want:  "spaced out",
		},
		{
			name:  "div endings become newlines",
			input: "<div>block one</div><div>block two</div>",
			want:  "block one\nblock two\n",
		},
		{
			name:  "nested tags",
			input: "<div><p><strong>bold</strong> text</p></div>",
			want:  "bold text\n\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripHTML(tt.input)
			if got != tt.want {
				t.Errorf("stripHTML(%q)\n  got:  %q\n  want: %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestDecodeBase64URL(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "simple text",
			input: "SGVsbG8gV29ybGQ",
			want:  "Hello World",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
		{
			name:  "invalid base64",
			input: "!!!not-base64!!!",
			want:  "",
		},
		{
			name:  "url-safe characters",
			input: "YS1i_2M-ZA",
			want:  "a-b\xffc>d",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decodeBase64URL(tt.input)
			if got != tt.want {
				t.Errorf("decodeBase64URL(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseDate(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{
			name:  "RFC1123Z",
			input: "Mon, 02 Jan 2006 15:04:05 -0700",
		},
		{
			name:  "RFC3339",
			input: "2026-06-14T10:30:00-04:00",
		},
		{
			name:  "RFC2822 with timezone name",
			input: "Mon, 2 Jan 2006 15:04:05 -0700 (MST)",
		},
		{
			name:  "day without leading zero",
			input: "Sat, 5 Jan 2026 09:00:00 +0000",
		},
		{
			name:    "empty string",
			input:   "",
			wantErr: true,
		},
		{
			name:    "garbage",
			input:   "not a date",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseDate(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("parseDate(%q) expected error, got %v", tt.input, result)
				}
				return
			}
			if err != nil {
				t.Errorf("parseDate(%q) unexpected error: %v", tt.input, err)
				return
			}
			if result.IsZero() {
				t.Errorf("parseDate(%q) returned zero time", tt.input)
			}
		})
	}
}

func TestDecodeEntity(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"&amp;", "&"},
		{"&lt;", "<"},
		{"&gt;", ">"},
		{"&quot;", "\""},
		{"&apos;", "'"},
		{"&nbsp;", " "},
		{"&unknown;", "&unknown;"},
	}

	for _, tt := range tests {
		got := decodeEntity(tt.input)
		if got != tt.want {
			t.Errorf("decodeEntity(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestParseDateFormats(t *testing.T) {
	// Verify specific known date strings parse to expected values
	input := "Sat, 14 Jun 2026 10:30:00 -0400"
	result, err := parseDate(input)
	if err != nil {
		t.Fatalf("parseDate(%q) error: %v", input, err)
	}
	if result.Year() != 2026 || result.Month() != time.June || result.Day() != 14 {
		t.Errorf("parseDate(%q) = %v, expected 2026-06-14", input, result)
	}
	if result.Hour() != 10 || result.Minute() != 30 {
		t.Errorf("parseDate(%q) time = %02d:%02d, expected 10:30", input, result.Hour(), result.Minute())
	}
}

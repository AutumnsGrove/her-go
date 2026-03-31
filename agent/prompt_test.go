package agent

import (
	"strings"
	"testing"
)

func TestReplaceBetweenMarkers(t *testing.T) {
	tests := []struct {
		name    string
		content string
		tag     string
		replace string
		want    string
	}{
		{
			name:    "basic replacement",
			content: "before\n<!-- BEGIN FOO -->\nold content\n<!-- END FOO -->\nafter",
			tag:     "FOO",
			replace: "new content",
			want:    "before\n<!-- BEGIN FOO -->\nnew content\n<!-- END FOO -->\nafter",
		},
		{
			name:    "missing begin marker",
			content: "no markers here",
			tag:     "FOO",
			replace: "new content",
			want:    "no markers here",
		},
		{
			name:    "missing end marker",
			content: "<!-- BEGIN FOO -->\nstuff",
			tag:     "FOO",
			replace: "new content",
			want:    "<!-- BEGIN FOO -->\nstuff",
		},
		{
			name:    "end before begin",
			content: "<!-- END FOO -->\n<!-- BEGIN FOO -->",
			tag:     "FOO",
			replace: "new",
			want:    "<!-- END FOO -->\n<!-- BEGIN FOO -->",
		},
		{
			name:    "multiline replacement",
			content: "<!-- BEGIN X -->\nold\n<!-- END X -->",
			tag:     "X",
			replace: "line1\nline2\nline3",
			want:    "<!-- BEGIN X -->\nline1\nline2\nline3\n<!-- END X -->",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := replaceBetweenMarkers(tt.content, tt.tag, tt.replace)
			if got != tt.want {
				t.Errorf("\ngot:  %q\nwant: %q", got, tt.want)
			}
		})
	}
}

func TestExpandToolSections(t *testing.T) {
	// Verify that expandToolSections replaces both markers.
	input := "preamble\n<!-- BEGIN HOT_TOOLS -->\nold\n<!-- END HOT_TOOLS -->\nmiddle\n<!-- BEGIN CATEGORY_TABLE -->\nold table\n<!-- END CATEGORY_TABLE -->\nend"
	result := expandToolSections(input)

	// Should still contain the markers.
	if got := result; got == input {
		t.Error("expandToolSections did not change the content")
	}

	// Preamble and end should be preserved.
	if result[:9] != "preamble\n" {
		t.Error("preamble was modified")
	}
	if result[len(result)-3:] != "end" {
		t.Error("end was modified")
	}

	// Should contain generated hot tools content.
	if !strings.Contains(result, "- **") {
		t.Error("expanded content should contain hot tool bullets")
	}

	// Should contain generated category table.
	if !strings.Contains(result, "| Category |") {
		t.Error("expanded content should contain category table header")
	}
}

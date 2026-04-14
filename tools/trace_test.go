package tools

import (
	"strings"
	"testing"
)

func TestFormatTrace(t *testing.T) {
	tests := []struct {
		name     string
		tool     string
		argsJSON string
		result   string
		wantSub  string // substring that must appear in output
	}{
		{
			name:     "think shows thought in italics",
			tool:     "think",
			argsJSON: `{"thought":"should I search?"}`,
			result:   "tool call complete",
			wantSub:  "🧠",
		},
		{
			name:     "think escapes HTML",
			tool:     "think",
			argsJSON: `{"thought":"is 2 < 3?"}`,
			result:   "tool call complete",
			wantSub:  "2 &lt; 3",
		},
		{
			name:     "done shows just emoji",
			tool:     "done",
			argsJSON: `{}`,
			result:   "tool call complete",
			wantSub:  "✅",
		},
		{
			name:     "save_fact shows fact details",
			tool:     "save_fact",
			argsJSON: `{"fact":"likes coffee","category":"preference","importance":5}`,
			result:   "saved fact ID=42",
			wantSub:  "category=preference",
		},
		{
			name:     "save_fact rejection shows reject emoji",
			tool:     "save_fact",
			argsJSON: `{"fact":"some fact","category":"other","importance":1}`,
			result:   "rejected: too short",
			wantSub:  "🚫",
		},
		{
			name:     "unknown tool uses default format",
			tool:     "unknown_tool",
			argsJSON: `{}`,
			result:   "some result",
			wantSub:  "🔧",
		},
		{
			name:     "web_search shows query",
			tool:     "web_search",
			argsJSON: `{"query":"best coffee in Portland"}`,
			result:   "**Summary:** Great coffee scene\n**Sources:**\n1. ...",
			wantSub:  "🔍",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatTrace(tt.tool, tt.argsJSON, tt.result)
			if !strings.Contains(got, tt.wantSub) {
				t.Errorf("FormatTrace(%q) = %q, want substring %q", tt.tool, got, tt.wantSub)
			}
			t.Logf("  %s → %s", tt.tool, got)
		})
	}
}

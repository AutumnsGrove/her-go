package agent

import (
	"strings"
	"testing"
)

// TestBuildContinuationSummary verifies that buildContinuationSummary
// strips HTML tags, joins lines, and caps output at ~500 chars.
// This summary gets injected as plain text into the agent's context
// at the start of each continuation window, so it must be readable
// without Telegram's HTML rendering.
func TestBuildContinuationSummary(t *testing.T) {
	tests := []struct {
		name       string
		traceLines []string
		wantSubs   []string // substrings that must appear
		wantAbsent []string // substrings that must NOT appear
		maxLen     int      // if > 0, output must not exceed this
	}{
		{
			name: "strips HTML bold and italic tags",
			traceLines: []string{
				"🧠 <b>think:</b> <i>should I search?</i>",
				"🔍 <b>web_search:</b> \"coffee Portland\"",
			},
			wantSubs:   []string{"think:", "should I search?", "web_search:"},
			wantAbsent: []string{"<b>", "</b>", "<i>", "</i>"},
		},
		{
			name: "unescapes HTML entities",
			traceLines: []string{
				"score &lt; 5 &amp; &gt; 2",
			},
			wantSubs:   []string{"score < 5 & > 2"},
			wantAbsent: []string{"&lt;", "&gt;", "&amp;"},
		},
		{
			name:       "empty trace lines returns empty string",
			traceLines: []string{},
			wantSubs:   []string{},
		},
		{
			name: "truncates at 500 chars",
			traceLines: []string{
				strings.Repeat("a", 600),
			},
			maxLen: 503, // 500 + "..."
		},
		{
			name: "joins multiple lines with newlines",
			traceLines: []string{
				"line one",
				"line two",
				"line three",
			},
			wantSubs: []string{"line one", "line two", "line three"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildContinuationSummary(tt.traceLines)

			for _, sub := range tt.wantSubs {
				if !strings.Contains(got, sub) {
					t.Errorf("missing expected substring %q in output: %q", sub, got)
				}
			}
			for _, sub := range tt.wantAbsent {
				if strings.Contains(got, sub) {
					t.Errorf("unexpected substring %q found in output: %q", sub, got)
				}
			}
			if tt.maxLen > 0 && len(got) > tt.maxLen {
				t.Errorf("output too long: got %d chars, want <= %d", len(got), tt.maxLen)
			}
		})
	}
}

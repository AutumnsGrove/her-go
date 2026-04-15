package agent

import "testing"

func TestParseClassifierResponse(t *testing.T) {
	tests := []struct {
		name        string
		response    string
		wantAllowed bool
		wantType    string
		wantReason  string
		wantRewrite string
	}{
		{
			name:        "plain SAVE",
			response:    "SAVE",
			wantAllowed: true,
			wantType:    "SAVE",
		},
		{
			name:        "SAVE with explanation",
			response:    "SAVE — real user preference",
			wantAllowed: true,
			wantType:    "SAVE",
		},
		{
			// FICTIONAL was removed — too many false positives on real past events.
			// The agent model's own judgment handles fiction-filtering well enough.
			// Like INFERRED, the parser no longer recognizes it → fail-open.
			name:        "FICTIONAL removed — fails open",
			response:    "FICTIONAL — in-game event from Elden Ring",
			wantAllowed: true,
			wantType:    "SAVE",
		},
		{
			// INFERRED was removed in Phase 4 — memory agent reads raw conversation
			// text so reasonable summarization is always acceptable. The parser
			// no longer recognizes INFERRED and falls through to the fail-open path.
			name:        "INFERRED no longer known — fails open",
			response:    `INFERRED REWRITE: "User adopted their cat Bean from a Portland shelter"`,
			wantAllowed: true,
			wantType:    "SAVE",
		},
		{
			// MOOD_NOT_FACT removed — mood tracking moved to junk drawer.
			name:        "MOOD_NOT_FACT removed — fails open",
			response:    "MOOD_NOT_FACT — transient frustration",
			wantAllowed: true,
			wantType:    "SAVE",
		},
		{
			name:        "LOW_VALUE hard reject",
			response:    "LOW_VALUE — too vague to be actionable",
			wantAllowed: false,
			wantType:    "LOW_VALUE",
			wantReason:  "too vague to be actionable",
		},
		{
			name:        "unparseable response fails open",
			response:    "I think this fact is fine to save",
			wantAllowed: true,
			wantType:    "SAVE",
		},
		{
			name:        "multiline FICTIONAL removed — fails open",
			response:    "FICTIONAL — game event\nThe fact describes beating a boss",
			wantAllowed: true,
			wantType:    "SAVE",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := parseClassifierResponse(tt.response)
			if v.Allowed != tt.wantAllowed {
				t.Errorf("Allowed = %v, want %v", v.Allowed, tt.wantAllowed)
			}
			if v.Type != tt.wantType {
				t.Errorf("Type = %q, want %q", v.Type, tt.wantType)
			}
			if tt.wantReason != "" && v.Reason != tt.wantReason {
				t.Errorf("Reason = %q, want %q", v.Reason, tt.wantReason)
			}
			if tt.wantRewrite != "" && v.Rewrite != tt.wantRewrite {
				t.Errorf("Rewrite = %q, want %q", v.Rewrite, tt.wantRewrite)
			}
		})
	}
}

func TestExtractRewrite(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		verdict string
		want    string
	}{
		{
			// extractRewrite is a pure string utility — it works for any verdict
			// name, even removed ones. Testing it with removed verdicts confirms
			// the extractor itself still works.
			name:    "quoted rewrite (utility test)",
			line:    `FICTIONAL REWRITE: "User prefers bleed builds"`,
			verdict: "FICTIONAL",
			want:    "User prefers bleed builds",
		},
		{
			name:    "unquoted rewrite",
			line:    `INFERRED REWRITE: User adopted cat Bean`,
			verdict: "INFERRED",
			want:    "User adopted cat Bean",
		},
		{
			name:    "no rewrite present",
			line:    "LOW_VALUE — too vague",
			verdict: "LOW_VALUE",
			want:    "",
		},
		{
			name:    "rewrite with mixed case",
			line:    `LOW_VALUE Rewrite: "User enjoys surreal fiction like Piranesi"`,
			verdict: "LOW_VALUE",
			want:    "User enjoys surreal fiction like Piranesi",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractRewrite(tt.line, tt.verdict)
			if got != tt.want {
				t.Errorf("extractRewrite() = %q, want %q", got, tt.want)
			}
		})
	}
}

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
			name:        "hard reject FICTIONAL",
			response:    "FICTIONAL — in-game event from Elden Ring",
			wantAllowed: false,
			wantType:    "FICTIONAL",
			wantReason:  "in-game event from Elden Ring",
		},
		{
			name:        "soft reject FICTIONAL with rewrite",
			response:    `FICTIONAL REWRITE: "User prefers playing as female V in Cyberpunk 2077"`,
			wantAllowed: false,
			wantType:    "FICTIONAL",
			wantRewrite: "User prefers playing as female V in Cyberpunk 2077",
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
			name:        "MOOD_NOT_FACT is always hard reject",
			response:    "MOOD_NOT_FACT — transient frustration",
			wantAllowed: false,
			wantType:    "MOOD_NOT_FACT",
			wantReason:  "transient frustration",
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
			name:        "multiline takes first line only",
			response:    "FICTIONAL — game event\nThe fact describes beating a boss",
			wantAllowed: false,
			wantType:    "FICTIONAL",
			wantReason:  "game event",
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
			name:    "quoted rewrite",
			line:    `FICTIONAL REWRITE: "User prefers bleed builds"`,
			verdict: "FICTIONAL",
			want:    "User prefers bleed builds",
		},
		{
			// extractRewrite is a string utility — it works for any verdict name,
			// even removed ones. The full classifier pipeline won't apply INFERRED
			// rewrites (it fails open on unknown verdicts), but the extraction
			// function itself is still valid code worth testing.
			name:    "unquoted rewrite (INFERRED removed but extractor still works)",
			line:    `INFERRED REWRITE: User adopted cat Bean`,
			verdict: "INFERRED",
			want:    "User adopted cat Bean",
		},
		{
			name:    "no rewrite present",
			line:    "FICTIONAL — game event",
			verdict: "FICTIONAL",
			want:    "",
		},
		{
			name:    "rewrite with mixed case",
			line:    `FICTIONAL Rewrite: "User likes FromSoft games"`,
			verdict: "FICTIONAL",
			want:    "User likes FromSoft games",
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

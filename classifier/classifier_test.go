package classifier

import (
	"strings"
	"testing"
)

func TestParseResponse(t *testing.T) {
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
			// INFERRED was removed — memory agent reads raw conversation text so
			// reasonable summarization is always acceptable.
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
			name:        "PASS allowed",
			response:    "PASS",
			wantAllowed: true,
			wantType:    "PASS",
		},
		{
			name:        "STYLE_ISSUE rejected",
			response:    "STYLE_ISSUE — opens with hollow affirmation",
			wantAllowed: false,
			wantType:    "STYLE_ISSUE",
			wantReason:  "opens with hollow affirmation",
		},
		{
			name:        "unparseable response fails open",
			response:    "I think this fact is fine to save",
			wantAllowed: true,
			wantType:    "SAVE",
		},
		{
			name:        "multiline — only first line checked",
			response:    "LOW_VALUE — too vague\nThe fact doesn't tell us anything useful",
			wantAllowed: false,
			wantType:    "LOW_VALUE",
			wantReason:  "too vague",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := parseResponse(tt.response)
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
			name:    "rewrite with mixed case keyword",
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

func TestRejectionMessage(t *testing.T) {
	t.Run("soft verdict with rewrite suggests text", func(t *testing.T) {
		v := Verdict{Allowed: false, Type: "FICTIONAL", Rewrite: "User prefers bleed builds in Elden Ring"}
		msg := RejectionMessage(v)
		if msg == "" {
			t.Error("expected non-empty rejection message")
		}
		if !strings.Contains(msg, "User prefers bleed builds") {
			t.Errorf("expected rewrite text in message, got: %s", msg)
		}
	})

	t.Run("unknown verdict type falls back to reason", func(t *testing.T) {
		v := Verdict{Allowed: false, Type: "UNKNOWN_VERDICT", Reason: "some reason"}
		msg := RejectionMessage(v)
		if !strings.Contains(msg, "some reason") {
			t.Errorf("expected reason in fallback message, got: %s", msg)
		}
	})
}

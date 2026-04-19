package mood

import (
	"math"
	"testing"
)

func TestScoreSignals_EmptyReturnsZero(t *testing.T) {
	if got := ScoreSignals(nil); got != 0 {
		t.Errorf("ScoreSignals(nil) = %v, want 0", got)
	}
	if got := ScoreSignals([]Turn{}); got != 0 {
		t.Errorf("ScoreSignals(empty) = %v, want 0", got)
	}
}

func TestScoreSignals_FirstPersonAffectHitsHard(t *testing.T) {
	turns := []Turn{{Role: "user", ScrubbedContent: "I'm exhausted today"}}
	got := ScoreSignals(turns)

	// 0.50 for first-person + 0.25 for "exhausted" = 0.75.
	want := 0.75
	if math.Abs(got-want) > 0.001 {
		t.Errorf("ScoreSignals = %v, want %v", got, want)
	}
}

func TestScoreSignals_IntensityBoost(t *testing.T) {
	turns := []Turn{{Role: "user", ScrubbedContent: "I am absolutely exhausted"}}
	got := ScoreSignals(turns)

	// 0.50 first-person + 0.25 affect + 0.15 intensity = 0.90.
	want := 0.90
	if math.Abs(got-want) > 0.001 {
		t.Errorf("ScoreSignals = %v, want %v", got, want)
	}
}

func TestScoreSignals_EmojiBoost(t *testing.T) {
	turns := []Turn{{Role: "user", ScrubbedContent: "kind of a rough day 😔"}}
	got := ScoreSignals(turns)

	// 0.25 affect word (rough) + 0.10 emoji = 0.35 (no first-person
	// frame, no intensity).
	want := 0.35
	if math.Abs(got-want) > 0.001 {
		t.Errorf("ScoreSignals = %v, want %v", got, want)
	}
}

func TestScoreSignals_CapsAtOne(t *testing.T) {
	turns := []Turn{{Role: "user", ScrubbedContent: "I am absolutely totally exhausted stressed worried 😭"}}
	if got := ScoreSignals(turns); got > 1.0 {
		t.Errorf("ScoreSignals = %v, should cap at 1.0", got)
	}
}

// TestScoreSignals_IgnoresAssistantContent is important: without this
// the bot's sympathetic reply ("I'm sorry you're so stressed") would
// inflate its own confidence and we'd get feedback loops.
func TestScoreSignals_IgnoresAssistantContent(t *testing.T) {
	turns := []Turn{
		{Role: "user", ScrubbedContent: "how do I center a div"},
		{Role: "assistant", ScrubbedContent: "I'm sorry you're so stressed about CSS"},
	}
	got := ScoreSignals(turns)
	if got > 0.01 {
		t.Errorf("ScoreSignals = %v, want ~0 (assistant text should be ignored)", got)
	}
}

// TestScoreSignals_WordBoundary ensures "happy" doesn't match
// "unhappy" (otherwise the sign of the signal would flip).
func TestScoreSignals_WordBoundary(t *testing.T) {
	// "unhappy" is not in our bare affect word list — but "happy"
	// is. We want "unhappy" to NOT hit the "happy" matcher.
	turns := []Turn{{Role: "user", ScrubbedContent: "I feel unhappy"}}
	got := ScoreSignals(turns)

	// First-person framing hits (0.50) but bare "happy" word should
	// NOT (because it's embedded in "unhappy"). So total = 0.50.
	want := 0.50
	if math.Abs(got-want) > 0.001 {
		t.Errorf("ScoreSignals = %v, want %v (unhappy should not match happy)", got, want)
	}
}

func TestContainsWord(t *testing.T) {
	tests := []struct {
		text, word string
		want       bool
	}{
		{"i'm happy today", "happy", true},
		{"i'm unhappy today", "happy", false}, // left boundary fails
		{"the happy-ish mood", "happy", true}, // '-' is not a word byte
		{"happyish", "happy", false},          // right boundary fails
		{"", "happy", false},
		{"happy", "happy", true}, // whole string
	}
	for _, tc := range tests {
		got := containsWord(tc.text, tc.word)
		if got != tc.want {
			t.Errorf("containsWord(%q, %q) = %v, want %v", tc.text, tc.word, got, tc.want)
		}
	}
}

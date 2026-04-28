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

// TestScoreSignals_ThirdPersonFraming covers "it feels like",
// "everything is" — indirect emotional language that the old pre-gate
// used to silently drop.
func TestScoreSignals_ThirdPersonFraming(t *testing.T) {
	tests := []struct {
		name string
		text string
		min  float64 // score must be at least this
	}{
		{"it feels like", "it feels like nothing matters anymore", 0.50},
		{"everything is heavy", "everything is just heavy right now", 0.50},
		{"things feel off", "things feel really off today", 0.50},
		{"been feeling down", "been feeling kind of down lately", 0.50},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			turns := []Turn{{Role: "user", ScrubbedContent: tc.text}}
			got := ScoreSignals(turns)
			if got < tc.min {
				t.Errorf("ScoreSignals(%q) = %.2f, want >= %.2f", tc.text, got, tc.min)
			}
		})
	}
}

// TestScoreSignals_PositiveAffect ensures the pre-gate catches joy
// and excitement, not just sadness.
func TestScoreSignals_PositiveAffect(t *testing.T) {
	tests := []struct {
		name string
		text string
		min  float64
	}{
		{"stoked", "I'm so stoked about this", 0.75},
		{"hyped", "feeling really hyped right now", 0.50},
		{"amazing day", "today was actually amazing", 0.25},
		{"blessed", "honestly feeling blessed", 0.50},
		{"pumped emoji", "let's go 🔥", 0.10},
		{"thrilled", "I am thrilled", 0.75},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			turns := []Turn{{Role: "user", ScrubbedContent: tc.text}}
			got := ScoreSignals(turns)
			if got < tc.min {
				t.Errorf("ScoreSignals(%q) = %.2f, want >= %.2f", tc.text, got, tc.min)
			}
		})
	}
}

// TestScoreSignals_MetaphoricalMood covers the kind of messages from
// real conversations that the old pre-gate missed — weather metaphors,
// "heavy" feelings, feeling trapped/stuck.
func TestScoreSignals_MetaphoricalMood(t *testing.T) {
	tests := []struct {
		name string
		text string
		min  float64
	}{
		{"heavy everything", "I can't really explain it, everything is just heavier than usual", 0.50},
		{"trapped", "I'm trapped there financially", 0.75},
		{"stuck", "feeling stuck and can't get out", 0.50},
		{"numb", "I've been pretty numb lately", 0.50},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			turns := []Turn{{Role: "user", ScrubbedContent: tc.text}}
			got := ScoreSignals(turns)
			if got < tc.min {
				t.Errorf("ScoreSignals(%q) = %.2f, want >= %.2f", tc.text, got, tc.min)
			}
		})
	}
}

// TestScoreSignals_SimRegressions covers specific messages from the
// mood-pregate-stress sim that were false negatives before vocabulary
// expansion. If either of these drops below the 0.15 threshold again,
// something regressed.
func TestScoreSignals_SimRegressions(t *testing.T) {
	tests := []struct {
		name string
		text string
		min  float64
	}{
		{
			"rain_gray_indirect",
			"it's been raining nonstop for three days and everything just feels... gray",
			0.15, // must pass the pre-gate threshold
		},
		{
			"absolutely_unreal_positive",
			"the new album dropped and it is absolutely unreal. i've had it on repeat for three hours straight",
			0.15,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			turns := []Turn{{Role: "user", ScrubbedContent: tc.text}}
			got := ScoreSignals(turns)
			if got < tc.min {
				t.Errorf("ScoreSignals(%q) = %.2f, want >= %.2f (sim regression)", tc.text, got, tc.min)
			}
		})
	}
}

// TestScoreSignals_NeutralStillLow makes sure expanded phrases don't
// cause false positives on genuinely non-emotional messages.
func TestScoreSignals_NeutralStillLow(t *testing.T) {
	tests := []struct {
		name string
		text string
		max  float64
	}{
		{"factual question", "how do I configure the database connection", 0.10},
		{"code discussion", "it feels like the struct needs a pointer receiver", 0.50},
		{"scheduling", "things are busy this week, can we meet thursday", 0.50},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			turns := []Turn{{Role: "user", ScrubbedContent: tc.text}}
			got := ScoreSignals(turns)
			if got > tc.max {
				t.Errorf("ScoreSignals(%q) = %.2f, want <= %.2f", tc.text, got, tc.max)
			}
		})
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

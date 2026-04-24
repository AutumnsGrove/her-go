package mood

import (
	"strings"
	"testing"
)

func TestBuildPrompt_IncludesVocabAndTranscript(t *testing.T) {
	v := Default()
	turns := []Turn{
		{Role: "user", ScrubbedContent: "I'm exhausted"},
		{Role: "assistant", ScrubbedContent: "That sounds rough."},
	}
	// Try loading the real .md file from the project root (one level up
	// from the mood/ package directory). Fall back to default if absent.
	template := loadMoodPrompt("..")
	recentMoods := []string{"#5: valence 3, [Stressed, Tired]"}
	p := buildPrompt(template, v, turns, recentMoods)

	// Vocabulary is injected so the model can only pick known labels.
	if !strings.Contains(p, "Stressed") {
		t.Errorf("prompt missing canonical label 'Stressed'")
	}
	if !strings.Contains(p, "Work") {
		t.Errorf("prompt missing canonical association 'Work'")
	}

	// Transcript is rendered with role labels.
	if !strings.Contains(p, "user: I'm exhausted") {
		t.Errorf("prompt missing user-labelled turn")
	}
	if !strings.Contains(p, "her: That sounds rough.") {
		t.Errorf("prompt missing assistant-labelled turn")
	}

	// JSON schema is explicit enough that a parser can anchor on it.
	for _, marker := range []string{`"valence"`, `"labels"`, `"skip"`, `"confidence"`} {
		if !strings.Contains(p, marker) {
			t.Errorf("prompt missing schema marker %q", marker)
		}
	}

	// Recent moods should appear in the prompt.
	if !strings.Contains(p, "#5: valence 3, [Stressed, Tired]") {
		t.Errorf("prompt missing recent mood context")
	}
}

func TestBuildPrompt_EmptyRecentMoods(t *testing.T) {
	v := Default()
	turns := []Turn{
		{Role: "user", ScrubbedContent: "I feel great!"},
	}
	template := loadMoodPrompt("..")
	p := buildPrompt(template, v, turns, nil)

	// When no recent moods, should show "None yet".
	if !strings.Contains(p, "None yet") {
		t.Errorf("prompt with empty recent moods should show 'None yet'")
	}
}

func TestParseInference_HappyPath(t *testing.T) {
	raw := `{"skip":false,"valence":3,"labels":["Sad"],"associations":["Work"],"note":"rough day","confidence":0.8,"signals":["exhausted"]}`
	inf, err := parseInference(raw)
	if err != nil {
		t.Fatalf("parseInference: %v", err)
	}
	if inf.Skip {
		t.Error("Skip = true, want false")
	}
	if inf.Valence != 3 {
		t.Errorf("Valence = %d, want 3", inf.Valence)
	}
	if len(inf.Labels) != 1 || inf.Labels[0] != "Sad" {
		t.Errorf("Labels = %v, want [Sad]", inf.Labels)
	}
	if inf.Confidence < 0.79 || inf.Confidence > 0.81 {
		t.Errorf("Confidence = %v, want ~0.8", inf.Confidence)
	}
}

// Some models wrap their JSON in markdown fences even when asked not
// to. parseInference must tolerate that — the agent prompt doesn't
// need a re-prompt just because of a formatting quirk.
func TestParseInference_TolerantToCodeFences(t *testing.T) {
	tests := []string{
		"```json\n{\"skip\":true,\"reason\":\"no signal\"}\n```",
		"```\n{\"skip\":true,\"reason\":\"no signal\"}\n```",
		"   {\"skip\":true,\"reason\":\"no signal\"}   ",
	}
	for i, raw := range tests {
		inf, err := parseInference(raw)
		if err != nil {
			t.Errorf("[%d] parseInference: %v", i, err)
			continue
		}
		if !inf.Skip {
			t.Errorf("[%d] Skip = false, want true", i)
		}
	}
}

func TestParseInference_EmptyErrors(t *testing.T) {
	if _, err := parseInference(""); err == nil {
		t.Error("parseInference(empty) returned nil error")
	}
	if _, err := parseInference("   \n   "); err == nil {
		t.Error("parseInference(whitespace) returned nil error")
	}
}

func TestParseInference_MalformedErrors(t *testing.T) {
	if _, err := parseInference("{this is not json"); err == nil {
		t.Error("parseInference(bad json) returned nil error")
	}
}

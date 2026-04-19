package mood

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDefault_LoadsEmbeddedYAML is the "this vocab file is intact"
// guardrail. If someone edits mood/vocab.yaml and breaks it, the
// embedded bytes ship with a broken build and Default() panics — we
// want a test-time failure first.
func TestDefault_LoadsEmbeddedYAML(t *testing.T) {
	v := Default()

	// Apple defines exactly 7 valence buckets.
	if len(v.Buckets) != 7 {
		t.Errorf("Buckets count = %d, want 7", len(v.Buckets))
	}
	for i := 1; i <= 7; i++ {
		if _, ok := v.Buckets[i]; !ok {
			t.Errorf("missing bucket %d in embedded vocab", i)
		}
	}

	// A handful of canonical labels better be present — a typo in the
	// YAML that drops one would be a silent disaster otherwise.
	for _, want := range []string{"Sad", "Calm", "Joyful"} {
		if !v.IsLabel(want) {
			t.Errorf("embedded vocab missing label %q", want)
		}
	}
	for _, want := range []string{"Work", "Family", "Weather"} {
		if !v.IsAssociation(want) {
			t.Errorf("embedded vocab missing association %q", want)
		}
	}
}

func TestLoadVocab_HappyPath(t *testing.T) {
	path := writeTempYAML(t, validVocabYAML)

	v, err := LoadVocab(path)
	if err != nil {
		t.Fatalf("LoadVocab: %v", err)
	}
	if got := v.Buckets[1].Label; got != "Awful" {
		t.Errorf("Buckets[1].Label = %q, want %q", got, "Awful")
	}
	if !v.IsLabel("Sad") {
		t.Error("expected label Sad")
	}
	if !v.IsAssociation("Work") {
		t.Error("expected association Work")
	}
}

func TestLoadVocab_MissingFileErrors(t *testing.T) {
	_, err := LoadVocab(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatal("LoadVocab(missing) returned nil error")
	}
	if !strings.Contains(err.Error(), "reading") {
		t.Errorf("err = %v, want substring 'reading'", err)
	}
}

func TestLoadVocab_BadYAMLErrors(t *testing.T) {
	path := writeTempYAML(t, "valence_buckets:\n  this: is not: valid yaml ::: at all\n")
	_, err := LoadVocab(path)
	if err == nil {
		t.Fatal("LoadVocab(bad yaml) returned nil error")
	}
}

func TestLoadVocab_WrongBucketCountErrors(t *testing.T) {
	// Five buckets, not seven.
	yaml := `valence_buckets:
  1: {label: "A", emoji: "X", color: "#000000"}
  2: {label: "B", emoji: "X", color: "#000000"}
  3: {label: "C", emoji: "X", color: "#000000"}
  4: {label: "D", emoji: "X", color: "#000000"}
  5: {label: "E", emoji: "X", color: "#000000"}
labels:
  unpleasant: [Sad]
  neutral: [Calm]
  pleasant: [Happy]
associations: [Work]
`
	path := writeTempYAML(t, yaml)
	_, err := LoadVocab(path)
	if err == nil {
		t.Fatal("LoadVocab(wrong count) returned nil error")
	}
	if !strings.Contains(err.Error(), "want 7") {
		t.Errorf("err = %v, want substring 'want 7'", err)
	}
}

func TestLoadVocab_EmptyEmojiErrors(t *testing.T) {
	yaml := strings.Replace(validVocabYAML, `emoji: "E1"`, `emoji: ""`, 1)
	path := writeTempYAML(t, yaml)
	_, err := LoadVocab(path)
	if err == nil {
		t.Fatal("LoadVocab(empty emoji) returned nil error")
	}
	if !strings.Contains(err.Error(), "emoji") {
		t.Errorf("err = %v, want substring 'emoji'", err)
	}
}

func TestLoadVocab_BadHexColorErrors(t *testing.T) {
	yaml := strings.Replace(validVocabYAML, `color: "#F68B22"`, `color: "tomato"`, 1)
	path := writeTempYAML(t, yaml)
	_, err := LoadVocab(path)
	if err == nil {
		t.Fatal("LoadVocab(bad color) returned nil error")
	}
	if !strings.Contains(err.Error(), "hex") {
		t.Errorf("err = %v, want substring 'hex'", err)
	}
}

// TestLoadVocab_LabelCrossingTiersErrors — if someone puts "Calm" in
// both neutral and pleasant, the wizard has to pick one. We fail loudly
// so the author fixes it rather than silently picking one.
func TestLoadVocab_LabelCrossingTiersErrors(t *testing.T) {
	yaml := `valence_buckets:
  1: {label: "A", emoji: "😩", color: "#000000"}
  2: {label: "B", emoji: "😟", color: "#000000"}
  3: {label: "C", emoji: "🙁", color: "#000000"}
  4: {label: "D", emoji: "😐", color: "#000000"}
  5: {label: "E", emoji: "🙂", color: "#000000"}
  6: {label: "F", emoji: "😊", color: "#000000"}
  7: {label: "G", emoji: "😄", color: "#000000"}
labels:
  unpleasant: [Sad, Calm]
  neutral: [Calm]
  pleasant: [Happy]
associations: [Work]
`
	path := writeTempYAML(t, yaml)
	_, err := LoadVocab(path)
	if err == nil {
		t.Fatal("LoadVocab(cross-tier label) returned nil error")
	}
	if !strings.Contains(err.Error(), "both") {
		t.Errorf("err = %v, want substring 'both'", err)
	}
}

func TestLoadVocab_DuplicateAssociationErrors(t *testing.T) {
	// Same word, different case → should still trip dedup.
	yaml := strings.Replace(validVocabYAML,
		`associations: [Work, Family]`,
		`associations: [Work, work]`, 1)
	path := writeTempYAML(t, yaml)
	_, err := LoadVocab(path)
	if err == nil {
		t.Fatal("LoadVocab(dup association) returned nil error")
	}
	if !strings.Contains(err.Error(), "duplicate association") {
		t.Errorf("err = %v, want substring 'duplicate association'", err)
	}
}

func TestTierForValence(t *testing.T) {
	v := Default()

	tests := []struct {
		valence  int
		wantTier Tier
		wantOK   bool
	}{
		{1, TierUnpleasant, true},
		{3, TierUnpleasant, true},
		{4, TierNeutral, true},
		{5, TierPleasant, true},
		{7, TierPleasant, true},
		{0, TierNeutral, false},   // out of range
		{8, TierNeutral, false},   // out of range
		{-1, TierNeutral, false},  // negative
	}
	for _, tc := range tests {
		tier, ok := v.TierForValence(tc.valence)
		if tier != tc.wantTier || ok != tc.wantOK {
			t.Errorf("TierForValence(%d) = (%s, %v), want (%s, %v)",
				tc.valence, tier, ok, tc.wantTier, tc.wantOK)
		}
	}
}

func TestLabelsForValence_FiltersByTier(t *testing.T) {
	v := Default()

	// Any "unpleasant" valence should get the same label set.
	low := v.LabelsForValence(1)
	mid := v.LabelsForValence(3)
	if len(low) != len(mid) {
		t.Errorf("valence 1 (%d labels) != valence 3 (%d labels) — same tier should match",
			len(low), len(mid))
	}

	// And it shouldn't include known "pleasant" labels.
	for _, l := range low {
		if l == "Joyful" {
			t.Errorf("unpleasant labels include %q — should be in pleasant tier", l)
		}
	}

	// Out-of-range valence returns no labels.
	if got := v.LabelsForValence(99); got != nil {
		t.Errorf("LabelsForValence(99) = %v, want nil", got)
	}
}

func TestLabelsForTier_AlphabeticalAndCopied(t *testing.T) {
	v := Default()
	pleasant := v.LabelsForTier(TierPleasant)

	// Alphabetical order.
	for i := 1; i < len(pleasant); i++ {
		if pleasant[i-1] > pleasant[i] {
			t.Errorf("labels not sorted: %q then %q", pleasant[i-1], pleasant[i])
		}
	}

	// Mutating the returned slice must not affect the next call.
	if len(pleasant) > 0 {
		pleasant[0] = "MUTATED"
	}
	fresh := v.LabelsForTier(TierPleasant)
	if len(fresh) > 0 && fresh[0] == "MUTATED" {
		t.Error("LabelsForTier returned a slice that shares backing array with vocab")
	}
}

func TestAssociations_OrderedAndCopied(t *testing.T) {
	v := Default()
	list := v.Associations()

	if len(list) < 5 {
		t.Errorf("associations count = %d, want >= 5", len(list))
	}
	// Mutation shouldn't affect the vocab.
	if len(list) > 0 {
		list[0] = "MUTATED"
	}
	fresh := v.Associations()
	if len(fresh) > 0 && fresh[0] == "MUTATED" {
		t.Error("Associations() returned a slice that shares backing array")
	}
}

// writeTempYAML is a test helper: write data to a temp file, return
// the path. Cleanup happens via t.TempDir automatically.
func writeTempYAML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vocab.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// validVocabYAML is a minimal-but-complete vocab file used by tests
// that want a known-good baseline to mutate.
const validVocabYAML = `valence_buckets:
  1: {label: "Awful",    emoji: "E1", color: "#6B4AC4"}
  2: {label: "Bad",      emoji: "😟", color: "#8A6CE8"}
  3: {label: "Meh",      emoji: "🙁", color: "#A98DF1"}
  4: {label: "Okay",     emoji: "😐", color: "#B0B0B0"}
  5: {label: "Decent",   emoji: "🙂", color: "#F5C26B"}
  6: {label: "Good",     emoji: "😊", color: "#F6A945"}
  7: {label: "Great",    emoji: "😄", color: "#F68B22"}
labels:
  unpleasant: [Sad, Anxious]
  neutral: [Calm]
  pleasant: [Happy, Grateful]
associations: [Work, Family]
`

package layers

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"her/memory"
)

// newLayerTestStore opens a tiny store — embedDim=0 (mood layer
// doesn't touch vec_moods).
func newLayerTestStore(t *testing.T) *memory.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "layer.db")
	store, err := memory.NewStore(dbPath, 0)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestBuildChatMood_EmptyStoreEmitsNothing(t *testing.T) {
	store := newLayerTestStore(t)
	res := buildChatMood(&LayerContext{Store: store})
	if res.Content != "" {
		t.Errorf("Content = %q, want empty for empty store", res.Content)
	}
}

// Below the minimum injection threshold (2), don't inject — a single
// stray inference is noise, not signal.
func TestBuildChatMood_SingleEntryEmitsNothing(t *testing.T) {
	store := newLayerTestStore(t)
	_, err := store.SaveMoodEntry(&memory.MoodEntry{
		Kind: memory.MoodKindMomentary, Valence: 3,
		Labels: []string{"Sad"}, Source: memory.MoodSourceInferred,
	})
	if err != nil {
		t.Fatal(err)
	}
	res := buildChatMood(&LayerContext{Store: store})
	if res.Content != "" {
		t.Errorf("Content non-empty for 1 entry; want empty until min threshold met")
	}
}

func TestBuildChatMood_TwoEntriesInjectsContext(t *testing.T) {
	store := newLayerTestStore(t)
	for i := 0; i < 2; i++ {
		_, err := store.SaveMoodEntry(&memory.MoodEntry{
			Kind:    memory.MoodKindMomentary,
			Valence: 3,
			Labels:  []string{"Sad"},
			Source:  memory.MoodSourceInferred,
			Confidence: 0.8,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	res := buildChatMood(&LayerContext{Store: store})
	if res.Content == "" {
		t.Fatal("Content empty; expected mood block with 2 entries")
	}
	if !strings.Contains(res.Content, "Recent mood") {
		t.Errorf("Content missing header 'Recent mood': %q", res.Content)
	}
	if !strings.Contains(res.Content, "valence 3/7") {
		t.Errorf("Content missing valence line: %q", res.Content)
	}
	if !strings.Contains(res.Content, "Sad") {
		t.Errorf("Content missing label 'Sad': %q", res.Content)
	}
}

func TestBuildChatMood_SourceTag(t *testing.T) {
	store := newLayerTestStore(t)
	// Two entries so we clear the min threshold.
	_, _ = store.SaveMoodEntry(&memory.MoodEntry{
		Kind: memory.MoodKindMomentary, Valence: 6,
		Labels: []string{"Happy"}, Source: memory.MoodSourceConfirmed,
	})
	_, _ = store.SaveMoodEntry(&memory.MoodEntry{
		Kind: memory.MoodKindMomentary, Valence: 5,
		Labels: []string{"Calm"}, Source: memory.MoodSourceInferred,
		Confidence: 0.78,
	})
	res := buildChatMood(&LayerContext{Store: store})

	if !strings.Contains(res.Content, "self-reported") {
		t.Errorf("Content missing 'self-reported' tag for confirmed entry: %q", res.Content)
	}
	if !strings.Contains(res.Content, "inferred, confidence 0.78") {
		t.Errorf("Content missing confidence tag for inferred entry: %q", res.Content)
	}
}

func TestBuildChatMood_DailyRollupShownWhenRecent(t *testing.T) {
	store := newLayerTestStore(t)
	_, _ = store.SaveMoodEntry(&memory.MoodEntry{
		Kind: memory.MoodKindDaily, Valence: 4,
		Labels: []string{"Content"}, Source: memory.MoodSourceInferred,
		Timestamp: time.Now().Add(-12 * time.Hour),
	})
	// One more momentary so the total hits the threshold.
	_, _ = store.SaveMoodEntry(&memory.MoodEntry{
		Kind: memory.MoodKindMomentary, Valence: 5, Labels: []string{"Happy"},
	})

	res := buildChatMood(&LayerContext{Store: store})
	if !strings.Contains(res.Content, "daily rollup") {
		t.Errorf("Content missing 'daily rollup' line: %q", res.Content)
	}
}

func TestBuildChatMood_DailyRollupHiddenWhenStale(t *testing.T) {
	store := newLayerTestStore(t)
	// Daily older than the 48h window — shouldn't appear.
	_, _ = store.SaveMoodEntry(&memory.MoodEntry{
		Kind: memory.MoodKindDaily, Valence: 4, Labels: []string{"Content"},
		Timestamp: time.Now().Add(-5 * 24 * time.Hour),
	})
	// Two momentary to push the threshold.
	for i := 0; i < 2; i++ {
		_, _ = store.SaveMoodEntry(&memory.MoodEntry{
			Kind: memory.MoodKindMomentary, Valence: 5, Labels: []string{"Happy"},
		})
	}

	res := buildChatMood(&LayerContext{Store: store})
	if strings.Contains(res.Content, "daily rollup") {
		t.Errorf("stale daily rollup leaked into Content: %q", res.Content)
	}
}

func TestBuildChatMood_DetailCountMatches(t *testing.T) {
	store := newLayerTestStore(t)
	for i := 0; i < 3; i++ {
		_, _ = store.SaveMoodEntry(&memory.MoodEntry{
			Kind: memory.MoodKindMomentary, Valence: 5, Labels: []string{"Happy"},
		})
	}
	res := buildChatMood(&LayerContext{Store: store})
	if res.Detail != "3 recent mood entries" {
		t.Errorf("Detail = %q, want '3 recent mood entries'", res.Detail)
	}
}

func TestHumanTime(t *testing.T) {
	now := time.Now()
	tests := []struct {
		offset time.Duration
		want   string
	}{
		{30 * time.Second, "just now"},
		{5 * time.Minute, "5 min ago"},
		{90 * time.Minute, "1 hr ago"},
		{25 * time.Hour, "1 days ago"},
	}
	for _, tc := range tests {
		got := humanTime(now.Add(-tc.offset))
		if got != tc.want {
			t.Errorf("humanTime(-%v) = %q, want %q", tc.offset, got, tc.want)
		}
	}
}

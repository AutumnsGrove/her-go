package mood

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"her/memory"
	"her/scheduler"
)

func TestComputeDailyRollup_MeanValenceRounded(t *testing.T) {
	base := time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC)
	entries := []memory.MoodEntry{
		{Valence: 6, Labels: []string{"Happy"}, Timestamp: base},
		{Valence: 5, Labels: []string{"Calm"}, Timestamp: base.Add(1 * time.Hour)},
		{Valence: 7, Labels: []string{"Joyful"}, Timestamp: base.Add(2 * time.Hour)},
	}
	got := computeDailyRollup(entries, base.Add(8*time.Hour))

	if got.Kind != memory.MoodKindDaily {
		t.Errorf("Kind = %q, want daily", got.Kind)
	}
	if got.Source != memory.MoodSourceInferred {
		t.Errorf("Source = %q, want inferred", got.Source)
	}
	// Mean of 6, 5, 7 is 6.
	if got.Valence != 6 {
		t.Errorf("Valence = %d, want 6", got.Valence)
	}
}

func TestComputeDailyRollup_TopLabelsByFrequency(t *testing.T) {
	base := time.Now()
	entries := []memory.MoodEntry{
		{Valence: 3, Labels: []string{"Stressed", "Overwhelmed"}, Timestamp: base},
		{Valence: 3, Labels: []string{"Stressed"}, Timestamp: base.Add(1 * time.Hour)},
		{Valence: 2, Labels: []string{"Stressed", "Tired"}, Timestamp: base.Add(2 * time.Hour)},
		{Valence: 4, Labels: []string{"Overwhelmed"}, Timestamp: base.Add(3 * time.Hour)},
	}
	got := computeDailyRollup(entries, base)

	// Stressed appears 3×, Overwhelmed 2×, Tired 1× — top 3 in that
	// order. (Order: desc count, ties alphabetical).
	want := []string{"Stressed", "Overwhelmed", "Tired"}
	if len(got.Labels) != len(want) {
		t.Fatalf("Labels len = %d, want %d", len(got.Labels), len(want))
	}
	for i, w := range want {
		if got.Labels[i] != w {
			t.Errorf("Labels[%d] = %q, want %q", i, got.Labels[i], w)
		}
	}
}

func TestComputeDailyRollup_AlphabeticalTieBreak(t *testing.T) {
	base := time.Now()
	// Three labels, each appearing once → all tied at frequency 1 →
	// alphabetical tie-break.
	entries := []memory.MoodEntry{
		{Valence: 4, Labels: []string{"Peaceful"}, Timestamp: base},
		{Valence: 4, Labels: []string{"Calm"}, Timestamp: base.Add(1 * time.Hour)},
		{Valence: 4, Labels: []string{"Relaxed"}, Timestamp: base.Add(2 * time.Hour)},
	}
	got := computeDailyRollup(entries, base)
	want := []string{"Calm", "Peaceful", "Relaxed"}
	for i, w := range want {
		if got.Labels[i] != w {
			t.Errorf("Labels[%d] = %q, want %q", i, got.Labels[i], w)
		}
	}
}

func TestComputeDailyRollup_TopAssociation(t *testing.T) {
	base := time.Now()
	entries := []memory.MoodEntry{
		{Valence: 3, Associations: []string{"Work", "Family"}, Timestamp: base},
		{Valence: 3, Associations: []string{"Work"}, Timestamp: base.Add(1 * time.Hour)},
		{Valence: 2, Associations: []string{"Work"}, Timestamp: base.Add(2 * time.Hour)},
	}
	got := computeDailyRollup(entries, base)
	if len(got.Associations) != 1 || got.Associations[0] != "Work" {
		t.Errorf("Associations = %v, want [Work]", got.Associations)
	}
}

// TestRollupHandler_ExecuteEndToEnd — real Store, the scheduler
// handler interface, a captured Send callback so we can verify the
// summary content without Telegram.
func TestRollupHandler_ExecuteEndToEnd(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "rollup.db")
	store, err := memory.NewStore(dbPath, 0)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Seed three momentary entries today.
	today := time.Now().Truncate(time.Hour)
	for _, v := range []int{3, 4, 5} {
		if _, err := store.SaveMoodEntry(&memory.MoodEntry{
			Kind: memory.MoodKindMomentary, Valence: v,
			Labels: []string{"Calm"}, Timestamp: today,
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	// Capture sends.
	var mu sync.Mutex
	var sends []string
	send := func(chatID int64, text string) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		sends = append(sends, text)
		return 999, nil
	}

	deps := &scheduler.Deps{
		Store:  store,
		ChatID: 42,
		Send:   send,
	}
	h := dailyRollupHandler{}

	if err := h.Execute(context.Background(), json.RawMessage(`{}`), deps); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// One daily entry exists now.
	dailies, _ := store.RecentMoodEntries(memory.MoodKindDaily, 5)
	if len(dailies) != 1 {
		t.Fatalf("daily entries = %d, want 1", len(dailies))
	}
	if dailies[0].Valence != 4 { // mean of 3,4,5 = 4
		t.Errorf("daily valence = %d, want 4", dailies[0].Valence)
	}

	// Summary message was sent.
	if len(sends) != 1 {
		t.Fatalf("send count = %d, want 1", len(sends))
	}
	if !strings.Contains(sends[0], "rollup") {
		t.Errorf("send text = %q, want substring 'rollup'", sends[0])
	}
}

func TestRollupHandler_SkipsWhenDailyAlreadyExists(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "rollup_dup.db")
	store, _ := memory.NewStore(dbPath, 0)
	t.Cleanup(func() { _ = store.Close() })

	now := time.Now()
	// Today's momentary entries.
	_, _ = store.SaveMoodEntry(&memory.MoodEntry{
		Kind: memory.MoodKindMomentary, Valence: 4, Labels: []string{"Calm"},
		Timestamp: now,
	})
	// Already-logged daily. Sitting today's start + 21h should land
	// inside startOfDay..now regardless of test-run time.
	_, _ = store.SaveMoodEntry(&memory.MoodEntry{
		Kind: memory.MoodKindDaily, Valence: 5, Labels: []string{"Happy"},
		Timestamp: now.Add(-1 * time.Minute),
	})

	h := dailyRollupHandler{}
	if err := h.Execute(context.Background(), nil, &scheduler.Deps{Store: store}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Still exactly one daily (the handler skipped, didn't double up).
	dailies, _ := store.RecentMoodEntries(memory.MoodKindDaily, 5)
	if len(dailies) != 1 {
		t.Errorf("dailies = %d, want 1 (handler should skip when one exists)", len(dailies))
	}
}

func TestRollupHandler_SkipsWhenNoEntries(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "rollup_empty.db")
	store, _ := memory.NewStore(dbPath, 0)
	t.Cleanup(func() { _ = store.Close() })

	h := dailyRollupHandler{}
	if err := h.Execute(context.Background(), nil, &scheduler.Deps{Store: store}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	dailies, _ := store.RecentMoodEntries(memory.MoodKindDaily, 5)
	if len(dailies) != 0 {
		t.Errorf("dailies = %d, want 0 (nothing to roll up)", len(dailies))
	}
}

func TestTopNByCount_StableTieBreak(t *testing.T) {
	counts := map[string]int{"a": 1, "c": 3, "b": 1, "d": 2, "e": 3}
	got := topNByCount(counts, 3)
	// Top three: c(3), e(3), d(2). c < e alphabetically.
	want := []string{"c", "e", "d"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("[%d] = %q, want %q", i, got[i], w)
		}
	}
}

func TestBuildRollupNote_HandlesZeroLabels(t *testing.T) {
	// A quiet day with no labels shouldn't crash the formatter.
	note := buildRollupNote(4, nil, nil, 0)
	if note == "" {
		t.Error("buildRollupNote returned empty string")
	}
	if !strings.Contains(note, "Neutral") {
		t.Errorf("note = %q, want 'Neutral' substring", note)
	}
}

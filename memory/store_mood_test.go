package memory

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newMoodTestStore opens a store with embedDim=0 (no vec_moods) —
// covers every non-KNN path.
func newMoodTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "mood_test.db")
	store, err := NewStore(dbPath, 0)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// newMoodTestStoreWithVec opens a store with a real embedding
// dimension so vec_moods works. KNN dedup tests use this.
func newMoodTestStoreWithVec(t *testing.T, dim int) *SQLiteStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "mood_vec_test.db")
	store, err := NewStore(dbPath, dim)
	if err != nil {
		t.Fatalf("NewStore(dim=%d): %v", dim, err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestSaveMoodEntry_RoundTrip(t *testing.T) {
	store := newMoodTestStore(t)

	entry := &MoodEntry{
		Timestamp:      time.Now().UTC().Truncate(time.Second),
		Kind:           MoodKindMomentary,
		Valence:        6,
		Labels:         []string{"Happy", "Grateful"},
		Associations:   []string{"Work", "Family"},
		Note:           "Got a good code review today",
		Source:         MoodSourceInferred,
		Confidence:     0.82,
		ConversationID: "tg_42_1736894400",
	}
	id, err := store.SaveMoodEntry(entry)
	if err != nil {
		t.Fatalf("SaveMoodEntry: %v", err)
	}
	if id == 0 {
		t.Fatal("SaveMoodEntry returned id=0")
	}

	got, err := store.LatestMoodEntry("")
	if err != nil {
		t.Fatalf("LatestMoodEntry: %v", err)
	}
	if got == nil {
		t.Fatal("LatestMoodEntry returned nil")
	}
	if got.ID != id {
		t.Errorf("ID = %d, want %d", got.ID, id)
	}
	if got.Valence != 6 {
		t.Errorf("Valence = %d, want 6", got.Valence)
	}
	if len(got.Labels) != 2 || got.Labels[0] != "Happy" {
		t.Errorf("Labels = %v, want [Happy Grateful]", got.Labels)
	}
	if len(got.Associations) != 2 || got.Associations[0] != "Work" {
		t.Errorf("Associations = %v, want [Work Family]", got.Associations)
	}
	if got.Note != "Got a good code review today" {
		t.Errorf("Note = %q", got.Note)
	}
	if got.Source != MoodSourceInferred {
		t.Errorf("Source = %q, want inferred", got.Source)
	}
	if got.Confidence < 0.81 || got.Confidence > 0.83 {
		t.Errorf("Confidence = %v, want ~0.82", got.Confidence)
	}
	if got.ConversationID != "tg_42_1736894400" {
		t.Errorf("ConversationID = %q, want %q", got.ConversationID, "tg_42_1736894400")
	}
}

func TestSaveMoodEntry_NilEntryErrors(t *testing.T) {
	store := newMoodTestStore(t)
	if _, err := store.SaveMoodEntry(nil); err == nil {
		t.Error("SaveMoodEntry(nil) returned nil error")
	}
}

func TestSaveMoodEntry_ValenceOutOfRangeErrors(t *testing.T) {
	store := newMoodTestStore(t)
	for _, v := range []int{0, 8, -1, 99} {
		_, err := store.SaveMoodEntry(&MoodEntry{Valence: v})
		if err == nil {
			t.Errorf("SaveMoodEntry(valence=%d) returned nil error", v)
		}
	}
}

// TestSaveMoodEntry_EmptyLabelsStoredAsJSONArray — the defaulting of
// empty slices to `[]` rather than `null` matters for two things:
// downstream scan always unmarshals a valid array, and the JSON
// column type is consistent for analytics.
func TestSaveMoodEntry_EmptyLabelsStoredAsJSONArray(t *testing.T) {
	store := newMoodTestStore(t)

	_, err := store.SaveMoodEntry(&MoodEntry{
		Valence: 4,
		// Labels and Associations deliberately nil.
	})
	if err != nil {
		t.Fatalf("SaveMoodEntry: %v", err)
	}

	var labels, associations string
	row := store.db.QueryRow(`SELECT labels, associations FROM mood_entries LIMIT 1`)
	if err := row.Scan(&labels, &associations); err != nil {
		t.Fatalf("scan raw: %v", err)
	}
	if labels != "[]" {
		t.Errorf("raw labels = %q, want %q", labels, "[]")
	}
	if associations != "[]" {
		t.Errorf("raw associations = %q, want %q", associations, "[]")
	}
}

func TestSaveMoodEntry_ZeroConversationIDStoredAsNull(t *testing.T) {
	store := newMoodTestStore(t)

	_, err := store.SaveMoodEntry(&MoodEntry{Valence: 4})
	if err != nil {
		t.Fatalf("SaveMoodEntry: %v", err)
	}
	var convID *int64
	if err := store.db.QueryRow(`SELECT conversation_id FROM mood_entries LIMIT 1`).Scan(&convID); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if convID != nil {
		t.Errorf("conversation_id = %v, want NULL", *convID)
	}
}

func TestSaveMoodEntry_DefaultsWhenFieldsZero(t *testing.T) {
	store := newMoodTestStore(t)

	id, err := store.SaveMoodEntry(&MoodEntry{Valence: 4})
	if err != nil {
		t.Fatalf("SaveMoodEntry: %v", err)
	}
	got, err := store.LatestMoodEntry("")
	if err != nil {
		t.Fatalf("LatestMoodEntry: %v", err)
	}
	if got.ID != id {
		t.Errorf("ID mismatch")
	}
	if got.Kind != MoodKindMomentary {
		t.Errorf("Kind = %q, want momentary (default)", got.Kind)
	}
	if got.Source != MoodSourceInferred {
		t.Errorf("Source = %q, want inferred (default)", got.Source)
	}
	if got.Timestamp.IsZero() {
		t.Error("Timestamp should be defaulted to now")
	}
}

func TestLatestMoodEntry_FiltersByKind(t *testing.T) {
	store := newMoodTestStore(t)

	// Two momentary, one daily. Daily should win when we ask for daily.
	_, err := store.SaveMoodEntry(&MoodEntry{Valence: 3, Kind: MoodKindMomentary, Note: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.SaveMoodEntry(&MoodEntry{Valence: 5, Kind: MoodKindDaily, Note: "d1"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.SaveMoodEntry(&MoodEntry{Valence: 4, Kind: MoodKindMomentary, Note: "m2"})
	if err != nil {
		t.Fatal(err)
	}

	got, err := store.LatestMoodEntry(MoodKindDaily)
	if err != nil {
		t.Fatalf("LatestMoodEntry(daily): %v", err)
	}
	if got == nil || got.Note != "d1" {
		t.Errorf("LatestMoodEntry(daily) = %+v, want d1", got)
	}

	got, err = store.LatestMoodEntry(MoodKindMomentary)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Note != "m2" {
		t.Errorf("LatestMoodEntry(momentary) = %+v, want m2", got)
	}
}

func TestLatestMoodEntry_EmptyReturnsNil(t *testing.T) {
	store := newMoodTestStore(t)
	got, err := store.LatestMoodEntry("")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("LatestMoodEntry on empty table = %v, want nil", got)
	}
}

func TestRecentMoodEntries_OrdersAndLimits(t *testing.T) {
	store := newMoodTestStore(t)

	base := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 5; i++ {
		_, err := store.SaveMoodEntry(&MoodEntry{
			Valence:   4,
			Note:      string(rune('A' + i)),
			Timestamp: base.Add(time.Duration(i) * time.Minute),
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	got, err := store.RecentMoodEntries("", 3)
	if err != nil {
		t.Fatalf("RecentMoodEntries: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	// Newest first.
	if got[0].Note != "E" || got[2].Note != "C" {
		t.Errorf("ordering = %q,%q,%q, want E,D,C",
			got[0].Note, got[1].Note, got[2].Note)
	}
}

func TestMoodEntriesInRange_Windowing(t *testing.T) {
	store := newMoodTestStore(t)

	base := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	times := []time.Time{
		base.Add(-2 * time.Hour), // before window
		base.Add(-30 * time.Minute),
		base.Add(15 * time.Minute),
		base.Add(3 * time.Hour), // after window
	}
	for i, tm := range times {
		_, err := store.SaveMoodEntry(&MoodEntry{
			Valence:   4,
			Note:      string(rune('A' + i)),
			Timestamp: tm,
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	got, err := store.MoodEntriesInRange("", base.Add(-1*time.Hour), base.Add(1*time.Hour))
	if err != nil {
		t.Fatalf("MoodEntriesInRange: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	// oldest-first
	if got[0].Note != "B" || got[1].Note != "C" {
		t.Errorf("range entries = %q,%q, want B,C", got[0].Note, got[1].Note)
	}
}

func TestDeleteMoodEntry(t *testing.T) {
	store := newMoodTestStore(t)

	id, err := store.SaveMoodEntry(&MoodEntry{Valence: 4, Note: "ephemeral"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteMoodEntry(id); err != nil {
		t.Fatalf("DeleteMoodEntry: %v", err)
	}
	got, err := store.LatestMoodEntry("")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("row still present after delete: %+v", got)
	}
}

// --- Embedding / KNN dedup (requires EmbedDimension > 0) ---------

// makeEmbedding produces a deterministic dim-sized vector for tests.
// Pass seed>0; tiny permutations shift the vector enough that cosine
// distances are measurable but predictable.
func makeEmbedding(dim int, seed float32) []float32 {
	v := make([]float32, dim)
	for i := range v {
		v[i] = seed + float32(i)*0.01
	}
	return v
}

func TestSaveMoodEntry_EmbeddingRoundTripsViaVecMoods(t *testing.T) {
	dim := 16
	store := newMoodTestStoreWithVec(t, dim)

	base := makeEmbedding(dim, 0.5)
	id, err := store.SaveMoodEntry(&MoodEntry{
		Valence:   3,
		Labels:    []string{"Sad"},
		Note:      "rough day",
		Embedding: base,
	})
	if err != nil {
		t.Fatalf("SaveMoodEntry: %v", err)
	}

	// Row should be present, and a KNN query with the SAME vector
	// should find itself with distance ~0.
	hits, err := store.SimilarMoodEntriesWithin(time.Now(), base, 24*time.Hour, 5)
	if err != nil {
		t.Fatalf("SimilarMoodEntriesWithin: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("KNN returned no hits for the exact vector we just saved")
	}
	if hits[0].ID != id {
		t.Errorf("top hit id = %d, want %d", hits[0].ID, id)
	}
	if hits[0].Distance > 0.01 {
		t.Errorf("top-hit distance = %v, want ~0", hits[0].Distance)
	}
}

func TestSimilarMoodEntriesWithin_RespectsTimeWindow(t *testing.T) {
	dim := 16
	store := newMoodTestStoreWithVec(t, dim)

	// Old entry (3 hours ago). Will match on vector but not time.
	oldVec := makeEmbedding(dim, 0.5)
	_, err := store.SaveMoodEntry(&MoodEntry{
		Valence:   3,
		Note:      "old",
		Embedding: oldVec,
		Timestamp: time.Now().Add(-3 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Fresh entry (5 min ago). Will match both.
	newVec := makeEmbedding(dim, 0.5)
	_, err = store.SaveMoodEntry(&MoodEntry{
		Valence:   3,
		Note:      "new",
		Embedding: newVec,
		Timestamp: time.Now().Add(-5 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}

	// 30-min window should find only the fresh entry.
	hits, err := store.SimilarMoodEntriesWithin(time.Now(), newVec, 30*time.Minute, 5)
	if err != nil {
		t.Fatalf("SimilarMoodEntriesWithin: %v", err)
	}
	for _, h := range hits {
		if h.Note == "old" {
			t.Error("old entry came back despite being outside the time window")
		}
	}
}

func TestSimilarMoodEntriesWithin_EmbedDimZeroReturnsNil(t *testing.T) {
	store := newMoodTestStore(t) // embedDim=0
	got, err := store.SimilarMoodEntriesWithin(time.Now(), []float32{0.1, 0.2}, time.Hour, 5)
	if err != nil {
		t.Fatalf("SimilarMoodEntriesWithin: %v", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil (no vec table)", got)
	}
}

// --- Pending mood proposals ----------------------------------------

func TestSavePendingMoodProposal_RoundTrip(t *testing.T) {
	store := newMoodTestStore(t)

	proposal := map[string]any{"valence": 3, "labels": []string{"Sad"}}
	raw, _ := json.Marshal(proposal)

	id, err := store.SavePendingMoodProposal(&PendingMoodProposal{
		TelegramChatID:    42,
		TelegramMessageID: 9001,
		ProposalJSON:      raw,
		ExpiresAt:         time.Now().Add(30 * time.Minute),
	})
	if err != nil {
		t.Fatalf("SavePendingMoodProposal: %v", err)
	}

	got, err := store.PendingMoodProposalByMessageID(42, 9001)
	if err != nil {
		t.Fatalf("PendingMoodProposalByMessageID: %v", err)
	}
	if got == nil || got.ID != id {
		t.Fatalf("round-trip failed: %v", got)
	}
	if got.Status != MoodProposalPending {
		t.Errorf("Status = %q, want pending", got.Status)
	}
	if !strings.Contains(string(got.ProposalJSON), "Sad") {
		t.Errorf("ProposalJSON = %s, want substring 'Sad'", got.ProposalJSON)
	}
}

func TestPendingMoodProposalByMessageID_UnknownReturnsNil(t *testing.T) {
	store := newMoodTestStore(t)
	got, err := store.PendingMoodProposalByMessageID(1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func TestDuePendingMoodProposals_OnlyReturnsExpiredPending(t *testing.T) {
	store := newMoodTestStore(t)

	// Three proposals: one expired, one not yet, one expired-but-
	// already-confirmed. Only the first should come back.
	now := time.Now()
	expired, err := store.SavePendingMoodProposal(&PendingMoodProposal{
		TelegramChatID:    1, TelegramMessageID: 100,
		ProposalJSON: json.RawMessage(`{}`),
		ExpiresAt:    now.Add(-5 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.SavePendingMoodProposal(&PendingMoodProposal{
		TelegramChatID:    1, TelegramMessageID: 101,
		ProposalJSON: json.RawMessage(`{}`),
		ExpiresAt:    now.Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	already, err := store.SavePendingMoodProposal(&PendingMoodProposal{
		TelegramChatID:    1, TelegramMessageID: 102,
		ProposalJSON: json.RawMessage(`{}`),
		ExpiresAt:    now.Add(-10 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdatePendingMoodProposalStatus(already, MoodProposalConfirmed); err != nil {
		t.Fatal(err)
	}

	due, err := store.DuePendingMoodProposals(now)
	if err != nil {
		t.Fatalf("DuePendingMoodProposals: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("got %d due, want 1", len(due))
	}
	if due[0].ID != expired {
		t.Errorf("due[0] id = %d, want %d", due[0].ID, expired)
	}
}

func TestUpdatePendingMoodProposalStatus_MissingIDErrors(t *testing.T) {
	store := newMoodTestStore(t)
	if err := store.UpdatePendingMoodProposalStatus(999, MoodProposalConfirmed); err == nil {
		t.Error("expected error for missing id, got nil")
	}
}

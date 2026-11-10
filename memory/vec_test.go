package memory

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

// vecTestStore opens a store at the given dimension in a temp dir.
func vecTestStore(t *testing.T, dim int, opts ...openOption) (*SQLiteStore, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "vec_test.db")
	store, err := NewStore(dbPath, dim, opts...)
	if err != nil {
		t.Fatalf("NewStore(%d): %v", dim, err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, dbPath
}

// fakeEmbedder returns deterministic, dimension-controlled vectors. A closure
// rather than a network client keeps these tests hermetic — which is the whole
// point of memory.Embedder being an interface.
func fakeEmbedder(dims int) Embedder {
	return embedderFunc(func(text string) ([]float32, error) {
		if text == "" {
			return nil, fmt.Errorf("empty input")
		}
		vec := make([]float32, dims)
		// A crude deterministic hash so different texts get different vectors.
		for i := range vec {
			vec[i] = float32((len(text)+i)%7) / 7
		}
		return vec, nil
	})
}

type embedderFunc func(string) ([]float32, error)

func (f embedderFunc) Embed(text string) ([]float32, error) { return f(text) }

// TestVecDimensionGuard_RejectsMismatch is the regression test for the exact
// bug this file exists to prevent: a database created with 768-dimension tables
// silently accepting (and then failing on) 1024-dimension vectors.
func TestVecDimensionGuard_RejectsMismatch(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "guard.db")

	if _, err := NewStore(dbPath, 768); err != nil {
		t.Fatalf("first open at 768: %v", err)
	}

	_, err := NewStore(dbPath, 1024)
	if err == nil {
		t.Fatal("expected a dimension mismatch error, got nil")
	}

	var mismatch *VecDimensionMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("expected *VecDimensionMismatchError, got %T: %v", err, err)
	}
	if mismatch.Existing != 768 || mismatch.Configured != 1024 {
		t.Errorf("mismatch reports %d→%d, want 768→1024", mismatch.Existing, mismatch.Configured)
	}
	if mismatch.Table != "vec_memories" {
		t.Errorf("mismatch.Table = %q, want vec_memories", mismatch.Table)
	}
}

// TestVecDimensionGuard_AllowsSameDimension makes sure the guard does not
// trip on the ordinary case of reopening an existing database.
func TestVecDimensionGuard_AllowsSameDimension(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "same.db")

	s1, err := NewStore(dbPath, 768)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := NewStore(dbPath, 768)
	if err != nil {
		t.Fatalf("reopen at the same dimension should succeed: %v", err)
	}
	defer s2.Close()
}

// TestVecDimensionGuard_OptOutIsHonoured documents the single escape hatch:
// `her reembed` opens the mismatched database so it can repair it.
func TestVecDimensionGuard_OptOutIsHonoured(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "optout.db")

	s1, err := NewStore(dbPath, 768)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	_ = s1.Close()

	s2, err := NewStore(dbPath, 1024, AllowVecDimensionMismatch())
	if err != nil {
		t.Fatalf("open with AllowVecDimensionMismatch: %v", err)
	}
	defer s2.Close()
}

// TestVecDimensionGuard_DisabledWhenZero means a store opened without embeddings
// (the common test setup) never fails on vector-table state.
func TestVecDimensionGuard_DisabledWhenZero(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "zero.db")

	s1, err := NewStore(dbPath, 768)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	_ = s1.Close()

	if _, err := NewStore(dbPath, 0); err != nil {
		t.Fatalf("dimension 0 must skip the guard: %v", err)
	}
}

// TestRebuildVecIndex_ChangesDimension covers the repair path: drop + recreate
// at the new width, then the guard is satisfied.
func TestRebuildVecIndex_ChangesDimension(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "rebuild.db")

	s1, err := NewStore(dbPath, 768)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	_ = s1.Close()

	s2, err := NewStore(dbPath, 1024, AllowVecDimensionMismatch())
	if err != nil {
		t.Fatalf("open for rebuild: %v", err)
	}
	if err := s2.RebuildVecIndex(); err != nil {
		t.Fatalf("RebuildVecIndex: %v", err)
	}
	dims, err := s2.VecIndexDimensions()
	if err != nil {
		t.Fatalf("VecIndexDimensions: %v", err)
	}
	for table, dim := range dims {
		if dim != 1024 {
			t.Errorf("%s = %d dims after rebuild, want 1024", table, dim)
		}
	}
	if err := s2.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// The repaired database now opens without the escape hatch.
	s3, err := NewStore(dbPath, 1024)
	if err != nil {
		t.Fatalf("open after rebuild should pass the guard: %v", err)
	}
	defer s3.Close()
}

// TestReembedAll_RewritesMemoriesAndMoods is the behavioural test for the
// backfill: rows written at 768 are re-embedded at 1024, blob caches included.
func TestReembedAll_RewritesMemoriesAndMoods(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "reembed.db")

	// Seed at the old dimension so the store accepts 768 vectors.
	old, err := NewStore(dbPath, 768)
	if err != nil {
		t.Fatalf("seed store: %v", err)
	}
	oldVec := make([]float32, 768)
	id, err := old.SaveMemory("Autumn has two cats", "fact", "user", 0, 3, oldVec, oldVec, "cats, pets", "", 0, "", "")
	if err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}
	moodID, err := old.SaveMoodEntry(&MoodEntry{
		Valence:    5,
		Labels:     []string{"calm"},
		Note:       "settled evening",
		Confidence: 0.9,
		Embedding:  oldVec,
	})
	if err != nil {
		t.Fatalf("SaveMoodEntry: %v", err)
	}
	_ = old.Close()

	// Repair at the new dimension.
	store, err := NewStore(dbPath, 1024, AllowVecDimensionMismatch())
	if err != nil {
		t.Fatalf("reopen for reembed: %v", err)
	}
	defer store.Close()
	if err := store.RebuildVecIndex(); err != nil {
		t.Fatalf("RebuildVecIndex: %v", err)
	}

	stats, err := store.ReembedAll(fakeEmbedder(1024), ReembedOptions{})
	if err != nil {
		t.Fatalf("ReembedAll: %v", err)
	}
	if stats.Memories != 1 || stats.TotalMemories != 1 {
		t.Errorf("memories: %d/%d, want 1/1", stats.Memories, stats.TotalMemories)
	}
	if stats.Moods != 1 || stats.TotalMoods != 1 {
		t.Errorf("moods: %d/%d, want 1/1", stats.Moods, stats.TotalMoods)
	}
	if stats.Failed != 0 {
		t.Errorf("Failed = %d, want 0", stats.Failed)
	}

	// The BLOB caches now hold 1024-dimension vectors. AllActiveMemories is the
	// reader that includes them (GetMemory deliberately does not — vectors are
	// derived caches, not part of the record).
	all, err := store.AllActiveMemories()
	if err != nil {
		t.Fatalf("AllActiveMemories: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("AllActiveMemories returned %d rows, want 1", len(all))
	}
	if len(all[0].Embedding) != 1024 || len(all[0].EmbeddingText) != 1024 {
		t.Errorf("memory vectors: tag=%d text=%d, want 1024/1024",
			len(all[0].Embedding), len(all[0].EmbeddingText))
	}

	// And the vec mirrors are searchable at the new width — a KNN query with a
	// 1024-dim vector must return the row instead of erroring or returning nil.
	hits, err := store.SemanticSearch(fakeVec(1024), 5)
	if err != nil {
		t.Fatalf("SemanticSearch after reembed: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("SemanticSearch returned no rows: vec_memories mirror was not repopulated")
	}
	if hits[0].ID != id {
		t.Errorf("top hit = %d, want %d", hits[0].ID, id)
	}
	_ = moodID
}

// TestReembedAll_IsResumable checks that a second run is a no-op in effect and
// still succeeds — the property that makes `her reembed` safe to re-run.
func TestReembedAll_IsResumable(t *testing.T) {
	store, _ := vecTestStore(t, 1024)
	vec := fakeVec(1024)
	if _, err := store.SaveMemory("Autumn is learning Go", "fact", "user", 0, 3, vec, vec, "go, programming", "", 0, "", ""); err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}

	for i := 0; i < 2; i++ {
		stats, err := store.ReembedAll(fakeEmbedder(1024), ReembedOptions{SkipMoods: true})
		if err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
		if stats.Memories != 1 {
			t.Errorf("run %d: memories = %d, want 1", i+1, stats.Memories)
		}
	}
}

// TestReembedAll_CountsEmbedFailures makes sure one bad row does not abandon
// the backfill: the failure is counted, the rest still process.
func TestReembedAll_CountsEmbedFailures(t *testing.T) {
	store, _ := vecTestStore(t, 1024)
	vec := fakeVec(1024)
	for _, text := range []string{"first fact", "second fact"} {
		if _, err := store.SaveMemory(text, "fact", "user", 0, 3, vec, vec, "tags", "", 0, "", ""); err != nil {
			t.Fatalf("SaveMemory: %v", err)
		}
	}

	calls := 0
	failing := embedderFunc(func(text string) ([]float32, error) {
		calls++
		if calls == 1 {
			return nil, fmt.Errorf("simulated provider failure")
		}
		return fakeVec(1024), nil
	})

	stats, err := store.ReembedAll(failing, ReembedOptions{SkipMoods: true})
	if err != nil {
		t.Fatalf("ReembedAll: %v", err)
	}
	if stats.Failed != 1 {
		t.Errorf("Failed = %d, want 1", stats.Failed)
	}
	if stats.Memories != 1 {
		t.Errorf("Memories = %d, want 1 (one row succeeded)", stats.Memories)
	}
}

// fakeVec builds a fixed-width vector for tests that need one without an
// embedder.
func fakeVec(dims int) []float32 {
	vec := make([]float32, dims)
	for i := range vec {
		vec[i] = 0.5
	}
	return vec
}

// countingEmbedder wraps fakeEmbedder and counts calls, so a test can assert how
// much (paid) embedding work a run actually did.
func countingEmbedder(dims int, calls *int) Embedder {
	inner := fakeEmbedder(dims)
	return embedderFunc(func(text string) ([]float32, error) {
		*calls++
		return inner.Embed(text)
	})
}

// With SkipCurrent, a re-run only pays for rows that still hold vectors from
// the old model. This is what makes `her reembed` resumable after a crash: rows
// finished by the first run are recognised and left alone.
func TestReembedAll_SkipCurrentResumesWhereItStopped(t *testing.T) {
	store, _ := vecTestStore(t, 1024)
	vec := fakeVec(1024)

	// One row already current (1024-dim vectors, mirrored), one with no vectors
	// at all (what a row from the old model looks like to this check).
	if _, err := store.SaveMemory("already current", "fact", "user", 0, 3, vec, vec, "tags", "", 0, "", ""); err != nil {
		t.Fatalf("SaveMemory current: %v", err)
	}
	if _, err := store.SaveMemory("still stale", "fact", "user", 0, 3, nil, nil, "tags", "", 0, "", ""); err != nil {
		t.Fatalf("SaveMemory stale: %v", err)
	}

	calls := 0
	stats, err := store.ReembedAll(countingEmbedder(1024, &calls), ReembedOptions{SkipMoods: true, SkipCurrent: true})
	if err != nil {
		t.Fatalf("ReembedAll: %v", err)
	}
	if stats.Memories != 1 || stats.Skipped != 1 {
		t.Errorf("memories=%d skipped=%d, want 1 re-embedded and 1 skipped", stats.Memories, stats.Skipped)
	}
	if calls != 2 { // two vectors (tags + text) for the one stale row, none for the current one
		t.Errorf("embedder called %d times, want 2", calls)
	}

	// Second pass: everything is current now, so no embedding work at all.
	calls = 0
	stats, err = store.ReembedAll(countingEmbedder(1024, &calls), ReembedOptions{SkipMoods: true, SkipCurrent: true})
	if err != nil {
		t.Fatalf("second ReembedAll: %v", err)
	}
	if calls != 0 || stats.Skipped != 2 {
		t.Errorf("second run: calls=%d skipped=%d, want 0 calls and 2 skipped", calls, stats.Skipped)
	}
}

// A row whose BLOB cache is current but whose vec_memories mirror row is missing
// (a crash between the two writes) must not be skipped: it would never show up
// in KNN search.
func TestReembedAll_SkipCurrentRepairsMissingMirror(t *testing.T) {
	store, _ := vecTestStore(t, 1024)
	vec := fakeVec(1024)
	id, err := store.SaveMemory("mirror went missing", "fact", "user", 0, 3, vec, vec, "tags", "", 0, "", "")
	if err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}
	if _, err := store.db.Exec(`DELETE FROM vec_memories WHERE rowid = ?`, id); err != nil {
		t.Fatalf("deleting mirror row: %v", err)
	}

	calls := 0
	stats, err := store.ReembedAll(countingEmbedder(1024, &calls), ReembedOptions{SkipMoods: true, SkipCurrent: true})
	if err != nil {
		t.Fatalf("ReembedAll: %v", err)
	}
	if stats.Memories != 1 || stats.Skipped != 0 {
		t.Errorf("memories=%d skipped=%d, want the row repaired, not skipped", stats.Memories, stats.Skipped)
	}
	hits, err := store.SemanticSearch(vec, 5)
	if err != nil || len(hits) == 0 {
		t.Fatalf("row is not searchable after repair: hits=%d err=%v", len(hits), err)
	}
}

// Re-embedding is bookkeeping, not an edit: it must not stamp updated_at, which
// consumers read as "this mood entry was refined".
func TestReembedAll_LeavesMoodUpdatedAtAlone(t *testing.T) {
	store, _ := vecTestStore(t, 1024)
	id, err := store.SaveMoodEntry(&MoodEntry{
		Valence: 5, Labels: []string{"calm"}, Note: "settled evening",
		Confidence: 0.9, Embedding: fakeVec(1024),
	})
	if err != nil {
		t.Fatalf("SaveMoodEntry: %v", err)
	}
	var before sql.NullString
	if err := store.db.QueryRow(`SELECT updated_at FROM mood_entries WHERE id = ?`, id).Scan(&before); err != nil {
		t.Fatalf("reading updated_at: %v", err)
	}

	stats, err := store.ReembedAll(fakeEmbedder(1024), ReembedOptions{})
	if err != nil {
		t.Fatalf("ReembedAll: %v", err)
	}
	if stats.Moods != 1 {
		t.Fatalf("Moods = %d, want 1", stats.Moods)
	}

	var after sql.NullString
	if err := store.db.QueryRow(`SELECT updated_at FROM mood_entries WHERE id = ?`, id).Scan(&after); err != nil {
		t.Fatalf("reading updated_at: %v", err)
	}
	if before != after {
		t.Errorf("updated_at changed from %v to %v; a re-embed must not mark the entry as refined", before, after)
	}
}

// A mood entry with no text has nothing to embed. That is not a provider
// failure, so it must not be counted as one (the CLI exits non-zero on Failed).
func TestReembedAll_EmptyMoodNoteIsSkippedNotFailed(t *testing.T) {
	store, _ := vecTestStore(t, 1024)
	if _, err := store.SaveMoodEntry(&MoodEntry{Valence: 4, Labels: nil, Note: "", Confidence: 0.5}); err != nil {
		t.Fatalf("SaveMoodEntry: %v", err)
	}
	stats, err := store.ReembedAll(fakeEmbedder(1024), ReembedOptions{})
	if err != nil {
		t.Fatalf("ReembedAll: %v", err)
	}
	if stats.Failed != 0 || stats.Skipped != 1 {
		t.Errorf("failed=%d skipped=%d, want 0 failed and 1 skipped", stats.Failed, stats.Skipped)
	}
}

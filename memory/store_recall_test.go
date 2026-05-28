package memory

import (
	"path/filepath"
	"testing"
)

func newRecallTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "recall_test.db")
	store, err := NewStore(dbPath, 0)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestMarkMemoriesRecalled(t *testing.T) {
	store := newRecallTestStore(t)

	// Save two memories.
	id1, err := store.SaveMemory("user likes tea", "preference", "user", 0, 5, nil, nil, "tea", "", 0)
	if err != nil {
		t.Fatalf("SaveMemory 1: %v", err)
	}
	id2, err := store.SaveMemory("user works at Cava", "work", "user", 0, 7, nil, nil, "work", "", 0)
	if err != nil {
		t.Fatalf("SaveMemory 2: %v", err)
	}

	// Mark both as recalled.
	if err := store.MarkMemoriesRecalled([]int64{id1, id2}); err != nil {
		t.Fatalf("MarkMemoriesRecalled: %v", err)
	}

	// Verify recall_count incremented.
	m1, err := store.GetMemory(id1)
	if err != nil || m1 == nil {
		t.Fatalf("GetMemory(%d): %v", id1, err)
	}
	if m1.RecallCount != 1 {
		t.Errorf("memory %d recall_count = %d, want 1", id1, m1.RecallCount)
	}
	if m1.LastRecalledAt.IsZero() {
		t.Error("expected non-zero LastRecalledAt")
	}

	// Mark again — count should increment to 2.
	if err := store.MarkMemoriesRecalled([]int64{id1}); err != nil {
		t.Fatalf("MarkMemoriesRecalled second: %v", err)
	}
	m1, _ = store.GetMemory(id1)
	if m1.RecallCount != 2 {
		t.Errorf("memory %d recall_count = %d, want 2", id1, m1.RecallCount)
	}

	// Empty slice should be a no-op.
	if err := store.MarkMemoriesRecalled(nil); err != nil {
		t.Fatalf("MarkMemoriesRecalled nil: %v", err)
	}
}

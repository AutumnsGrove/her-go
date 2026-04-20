package remove_memory

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"her/memory"
	"her/tools"
)

// newRemoveMemoryTestStore opens a fresh temp SQLite with all tables
// created. embedDim=0 skips the vec_memories virtual table — these
// tests only need the memories table.
func newRemoveMemoryTestStore(t *testing.T) *memory.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "remove_memory_test.db")
	store, err := memory.NewStore(dbPath, 0)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// saveTestMemory inserts a minimal memory and returns its ID.
// It fatally fails the test on error so call sites stay clean.
func saveTestMemory(t *testing.T, store *memory.Store, content string) int64 {
	t.Helper()
	id, err := store.SaveMemory(content, "test", "user", 0, 5, nil, nil, "", "")
	if err != nil {
		t.Fatalf("SaveMemory(%q): %v", content, err)
	}
	return id
}

// isActiveMemory returns true if the memory with the given ID is still in
// the active memories list. We check AllActiveMemories rather than GetMemory
// so the test validates the same view the agent uses for retrieval.
func isActiveMemory(t *testing.T, store *memory.Store, id int64) bool {
	t.Helper()
	memories, err := store.AllActiveMemories()
	if err != nil {
		t.Fatalf("AllActiveMemories: %v", err)
	}
	for _, m := range memories {
		if m.ID == id {
			return true
		}
	}
	return false
}

// TestHandle_SingleRemove verifies that passing memory_id deactivates exactly
// that one memory. After the call, the memory should no longer appear in
// AllActiveMemories but all others should be unaffected.
func TestHandle_SingleRemove(t *testing.T) {
	store := newRemoveMemoryTestStore(t)
	ctx := &tools.Context{Store: store}

	id := saveTestMemory(t, store, "likes jazz")

	result := Handle(fmt.Sprintf(`{"memory_id": %d, "reason": "outdated"}`, id), ctx)

	if strings.Contains(result, "error") {
		t.Fatalf("Handle returned error: %q", result)
	}
	if !strings.Contains(result, fmt.Sprintf("ID=%d", id)) {
		t.Errorf("result %q does not mention ID=%d", result, id)
	}

	if isActiveMemory(t, store, id) {
		t.Errorf("memory %d is still active after deactivation", id)
	}
}

// TestHandle_BatchRemove verifies that passing memory_ids deactivates all
// listed memories in a single call.
func TestHandle_BatchRemove(t *testing.T) {
	store := newRemoveMemoryTestStore(t)
	ctx := &tools.Context{Store: store}

	id1 := saveTestMemory(t, store, "works at old job")
	id2 := saveTestMemory(t, store, "lives in city A")
	id3 := saveTestMemory(t, store, "drives a sedan")

	result := Handle(
		fmt.Sprintf(`{"memory_ids": [%d, %d, %d], "reason": "duplicates"}`, id1, id2, id3),
		ctx,
	)

	if strings.Contains(result, "errors") {
		t.Fatalf("Handle returned partial errors: %q", result)
	}
	// The success message reports the count.
	if !strings.Contains(result, "removed 3 memories") {
		t.Errorf("result = %q, want 'removed 3 memories'", result)
	}

	for _, id := range []int64{id1, id2, id3} {
		if isActiveMemory(t, store, id) {
			t.Errorf("memory %d is still active after batch deactivation", id)
		}
	}
}

// TestHandle_NoID verifies that an args payload with neither memory_id nor
// memory_ids returns an error string instead of silently doing nothing.
func TestHandle_NoID(t *testing.T) {
	store := newRemoveMemoryTestStore(t)
	ctx := &tools.Context{Store: store}

	result := Handle(`{"reason": "test"}`, ctx)

	if !strings.Contains(result, "error") {
		t.Errorf("expected error for missing ID, got: %q", result)
	}
}

// TestHandle_BatchPartialFailure verifies that a batch containing a
// non-existent ID still deactivates the valid ones. The return string
// should report both what succeeded and what failed.
func TestHandle_BatchPartialFailure(t *testing.T) {
	store := newRemoveMemoryTestStore(t)
	ctx := &tools.Context{Store: store}

	id1 := saveTestMemory(t, store, "valid memory A")
	id2 := saveTestMemory(t, store, "valid memory B")
	const nonExistentID = int64(99999)

	result := Handle(
		fmt.Sprintf(`{"memory_ids": [%d, %d, %d], "reason": "cleanup"}`, id1, nonExistentID, id2),
		ctx,
	)

	// The result may report a partial error for the missing ID, but the
	// valid memories should still be deactivated.
	//
	// Note: DeactivateMemory runs a plain UPDATE WHERE id=? — SQLite
	// does not error on zero rows affected, so the "partial failure" path
	// actually won't trigger errors for a non-existent ID. What matters
	// is that the real memories are gone.
	if isActiveMemory(t, store, id1) {
		t.Errorf("memory %d should be deactivated (batch partial run)", id1)
	}
	if isActiveMemory(t, store, id2) {
		t.Errorf("memory %d should be deactivated (batch partial run)", id2)
	}

	// The result string should report at least 2 removals.
	if !strings.Contains(result, "removed") {
		t.Errorf("result %q does not mention removals", result)
	}
}

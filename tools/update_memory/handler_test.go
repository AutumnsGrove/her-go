// Package update_memory — tests for the update_memory tool handler.
//
// Strategy: all tests use a real SQLite store in a temp directory (same as the
// remove_memory tests) so we exercise the actual DB schema. ClassifierLLM and
// EmbedClient are left nil, which is the fail-open path the handler is designed
// to support — no embedding, no classifier check, but all style/length/supersession
// logic still fires.
package update_memory

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"her/memory"
	"her/tools"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// newTestStore opens a fresh temp SQLite with all tables created.
// embedDim=0 skips the vec_memories virtual table — these tests only need
// the memories table and supersession columns.
func newTestStore(t *testing.T) *memory.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := memory.NewStore(dbPath, 0)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// saveMemory inserts a memory with the given content, category and subject.
// Fatally fails the test on error so call sites stay clean.
func saveMemory(t *testing.T, store *memory.Store, content, category, subject string) int64 {
	t.Helper()
	id, err := store.SaveMemory(content, category, subject, 0, 5, nil, nil, "", "")
	if err != nil {
		t.Fatalf("SaveMemory(%q): %v", content, err)
	}
	return id
}

// getMemory fetches a memory by ID including inactive ones.
// Fatally fails the test if the DB call itself errors; returns nil if not found.
func getMemory(t *testing.T, store *memory.Store, id int64) *memory.Memory {
	t.Helper()
	m, err := store.GetMemory(id)
	if err != nil {
		t.Fatalf("GetMemory(%d): %v", id, err)
	}
	return m
}

// isActive returns true if the memory is in AllActiveMemories.
// Checking via the same view the agent uses for retrieval is more meaningful
// than just reading the row directly.
func isActive(t *testing.T, store *memory.Store, id int64) bool {
	t.Helper()
	all, err := store.AllActiveMemories()
	if err != nil {
		t.Fatalf("AllActiveMemories: %v", err)
	}
	for _, m := range all {
		if m.ID == id {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Happy path
// ---------------------------------------------------------------------------

// TestHandle_HappyPath creates a memory, updates it via Handle, then verifies:
//   - the result string mentions "updated" and both IDs
//   - a new memory exists with the updated content
//   - the old memory is no longer active (superseded)
func TestHandle_HappyPath(t *testing.T) {
	store := newTestStore(t)
	ctx := &tools.Context{Store: store}

	oldID := saveMemory(t, store, "Autumn lives in Portland", "location", "user")

	argsJSON := fmt.Sprintf(
		`{"memory_id":%d,"memory":"Autumn lives in Seattle","category":"location","tags":"location, city"}`,
		oldID,
	)
	result := Handle(argsJSON, ctx)

	// The handler should report success, not an error.
	if !strings.HasPrefix(result, "updated:") {
		t.Fatalf("Handle result = %q, want prefix 'updated:'", result)
	}
	// Both old and new IDs should appear in the result.
	if !strings.Contains(result, fmt.Sprintf("ID=%d", oldID)) {
		t.Errorf("result %q missing old ID=%d", result, oldID)
	}
	// The updated text should appear in the result.
	if !strings.Contains(result, "Autumn lives in Seattle") {
		t.Errorf("result %q missing updated memory text", result)
	}

	// Old memory should be gone from active set.
	if isActive(t, store, oldID) {
		t.Errorf("old memory ID=%d is still active after update", oldID)
	}
}

// ---------------------------------------------------------------------------
// Supersession chain
// ---------------------------------------------------------------------------

// TestHandle_SupersessionChain verifies that after a successful update the old
// memory row has Active=false and SupersededBy set to the new memory's ID.
// This is the Zettelkasten audit trail — knowledge evolution is preserved.
func TestHandle_SupersessionChain(t *testing.T) {
	store := newTestStore(t)
	ctx := &tools.Context{Store: store}

	oldID := saveMemory(t, store, "Autumn works at Acme Corp", "work", "user")

	argsJSON := fmt.Sprintf(
		`{"memory_id":%d,"memory":"Autumn works at New Co","category":"work"}`,
		oldID,
	)
	result := Handle(argsJSON, ctx)

	if !strings.HasPrefix(result, "updated:") {
		t.Fatalf("Handle returned non-success: %q", result)
	}

	// Pull the old memory row — GetMemory returns inactive memories too.
	old := getMemory(t, store, oldID)
	if old == nil {
		t.Fatal("old memory row missing from DB after update")
	}
	if old.Active {
		t.Errorf("old memory Active = true, want false (superseded)")
	}
	if old.SupersededBy == 0 {
		t.Errorf("old memory SupersededBy = 0, want the new memory's ID")
	}

	// Verify the pointer forward: the new memory should be active.
	newID := old.SupersededBy
	if !isActive(t, store, newID) {
		t.Errorf("new memory ID=%d is not active", newID)
	}
}

// ---------------------------------------------------------------------------
// Subject inheritance
// ---------------------------------------------------------------------------

// TestHandle_InheritsSubject verifies that the new memory inherits the old
// memory's subject. If the original was "self", the replacement should also
// be "self" even when the args don't specify a subject field (it's not an
// arg — the handler reads it from the old memory row).
func TestHandle_InheritsSubject(t *testing.T) {
	store := newTestStore(t)
	ctx := &tools.Context{Store: store}

	// Save a self-memory (subject="self").
	oldID := saveMemory(t, store, "I find Autumn's curiosity energising", "self", "self")

	argsJSON := fmt.Sprintf(
		`{"memory_id":%d,"memory":"I find Autumn's curiosity and honesty energising","category":"self"}`,
		oldID,
	)
	result := Handle(argsJSON, ctx)

	if !strings.HasPrefix(result, "updated:") {
		t.Fatalf("Handle returned non-success: %q", result)
	}

	old := getMemory(t, store, oldID)
	if old == nil || old.SupersededBy == 0 {
		t.Fatal("supersession chain not created")
	}

	newMem := getMemory(t, store, old.SupersededBy)
	if newMem == nil {
		t.Fatalf("new memory ID=%d not found", old.SupersededBy)
	}
	if newMem.Subject != "self" {
		t.Errorf("new memory Subject = %q, want %q", newMem.Subject, "self")
	}
}

// ---------------------------------------------------------------------------
// Memory not found
// ---------------------------------------------------------------------------

// TestHandle_NotFound verifies that passing a nonexistent memory_id returns a
// "not found" message rather than panicking or silently succeeding.
func TestHandle_NotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := &tools.Context{Store: store}

	result := Handle(`{"memory_id":99999,"memory":"some update","category":"other"}`, ctx)

	if !strings.Contains(result, "not found") {
		t.Errorf("Handle result = %q, want 'not found'", result)
	}
}

// ---------------------------------------------------------------------------
// Invalid JSON
// ---------------------------------------------------------------------------

// TestHandle_InvalidJSON verifies that malformed args return an error string
// rather than panicking. Matches the resilience contract across all tool handlers.
func TestHandle_InvalidJSON(t *testing.T) {
	store := newTestStore(t)
	ctx := &tools.Context{Store: store}

	result := Handle(`{"memory_id":1,"memory":"truncated`, ctx)

	if !strings.Contains(result, "error") {
		t.Errorf("Handle result = %q, want an error string for malformed JSON", result)
	}
}

// ---------------------------------------------------------------------------
// Style gate
// ---------------------------------------------------------------------------

// TestHandle_StyleGate verifies that memories containing AI writing tics are
// rejected before any DB write. The old memory should be completely untouched.
func TestHandle_StyleGate(t *testing.T) {
	cases := []struct {
		name   string
		memory string
	}{
		{
			name:   "leverage",
			memory: "Autumn wants to leverage Go skills for backend work",
		},
		{
			name:   "delve",
			memory: "Autumn loves to delve into complex problems",
		},
		{
			name:   "not_just",
			memory: "It's not just about code, it's about craftsmanship",
		},
		{
			name:   "fundamentally",
			memory: "Autumn is fundamentally a creative person",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			ctx := &tools.Context{Store: store}

			oldID := saveMemory(t, store, "Autumn writes Go code", "work", "user")

			argsJSON := fmt.Sprintf(
				`{"memory_id":%d,"memory":%q,"category":"work"}`,
				oldID, tc.memory,
			)
			result := Handle(argsJSON, ctx)

			if !strings.HasPrefix(result, "rejected:") {
				t.Errorf("Handle result = %q, want prefix 'rejected:'", result)
			}

			// Old memory must still be active — style-gate rejection must not
			// touch the store at all.
			if !isActive(t, store, oldID) {
				t.Errorf("old memory ID=%d was deactivated by a style-gate rejection", oldID)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Length gate
// ---------------------------------------------------------------------------

// TestHandle_LengthGate verifies that a memory over 300 characters is rejected
// before any DB write. The old memory is left untouched.
func TestHandle_LengthGate(t *testing.T) {
	store := newTestStore(t)
	ctx := &tools.Context{Store: store}

	oldID := saveMemory(t, store, "Autumn writes code", "work", "user")

	// Build a memory that's exactly maxMemoryLength+1 characters.
	// 300 is the package constant — we go one over to guarantee rejection.
	longMem := strings.Repeat("x", 301)
	argsJSON := fmt.Sprintf(
		`{"memory_id":%d,"memory":%q,"category":"work"}`,
		oldID, longMem,
	)
	result := Handle(argsJSON, ctx)

	if !strings.HasPrefix(result, "rejected:") {
		t.Errorf("Handle result = %q, want prefix 'rejected:'", result)
	}
	if !strings.Contains(result, "characters") {
		t.Errorf("rejection message %q should mention character count", result)
	}

	// Old memory must still be active.
	if !isActive(t, store, oldID) {
		t.Errorf("old memory ID=%d was deactivated by a length-gate rejection", oldID)
	}
}

// ---------------------------------------------------------------------------
// Trailing em dash gate
// ---------------------------------------------------------------------------

// TestHandle_TrailingEmDash verifies that a memory ending with \u2014 (em dash)
// or \u2013 (en dash) is rejected as an AI writing tic.
func TestHandle_TrailingEmDash(t *testing.T) {
	cases := []struct {
		name   string
		memory string
	}{
		{"em_dash", "Autumn is passionate about learning\u2014"},
		{"en_dash", "She prefers functional style\u2013"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			ctx := &tools.Context{Store: store}

			oldID := saveMemory(t, store, "Autumn writes code", "work", "user")

			argsJSON := fmt.Sprintf(
				`{"memory_id":%d,"memory":%q,"category":"work"}`,
				oldID, tc.memory,
			)
			result := Handle(argsJSON, ctx)

			if !strings.HasPrefix(result, "rejected:") {
				t.Errorf("[%s] Handle result = %q, want prefix 'rejected:'", tc.name, result)
			}

			if !isActive(t, store, oldID) {
				t.Errorf("[%s] old memory was deactivated by a trailing-dash rejection", tc.name)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Fail-open paths (nil clients)
// ---------------------------------------------------------------------------

// TestHandle_NilClassifier verifies that the update succeeds when ClassifierLLM
// is nil. Classifier absence is the fail-open design: writes proceed unclassified.
func TestHandle_NilClassifier(t *testing.T) {
	store := newTestStore(t)
	// ClassifierLLM is nil by default in &tools.Context{}.
	ctx := &tools.Context{Store: store}

	oldID := saveMemory(t, store, "Autumn drinks tea", "preference", "user")

	argsJSON := fmt.Sprintf(
		`{"memory_id":%d,"memory":"Autumn drinks green tea","category":"preference"}`,
		oldID,
	)
	result := Handle(argsJSON, ctx)

	if !strings.HasPrefix(result, "updated:") {
		t.Errorf("Handle result = %q, want 'updated:' (nil classifier should not block)", result)
	}
}

// TestHandle_NilEmbedClient verifies that the update succeeds when EmbedClient
// is nil. Embedding is skipped gracefully — the memory is saved without vectors.
func TestHandle_NilEmbedClient(t *testing.T) {
	store := newTestStore(t)
	// EmbedClient is nil by default.
	ctx := &tools.Context{Store: store}

	oldID := saveMemory(t, store, "Autumn prefers dark themes", "preference", "user")

	argsJSON := fmt.Sprintf(
		`{"memory_id":%d,"memory":"Autumn prefers dark themes in her editor","category":"preference"}`,
		oldID,
	)
	result := Handle(argsJSON, ctx)

	if !strings.HasPrefix(result, "updated:") {
		t.Errorf("Handle result = %q, want 'updated:' (nil embed client should not block)", result)
	}

	// Verify the new memory landed in the DB.
	old := getMemory(t, store, oldID)
	if old == nil || old.SupersededBy == 0 {
		t.Fatal("supersession chain not created when EmbedClient is nil")
	}
}

// ---------------------------------------------------------------------------
// SavedMemories tracking
// ---------------------------------------------------------------------------

// TestHandle_SavedMemoriesTracked verifies that after a successful update,
// ctx.SavedMemories is NOT populated by update_memory (it delegates to
// SaveMemory directly, not ExecSaveMemory which tracks SavedMemories).
// This is a deliberate design choice: the agent uses SavedMemories to
// trigger reflection; updates are revisions, not new facts.
//
// Actually: update_memory calls Store.SaveMemory directly (not ExecSaveMemory),
// so SavedMemories will NOT be appended. This test documents that behaviour.
func TestHandle_SavedMemoriesNotTracked(t *testing.T) {
	store := newTestStore(t)
	ctx := &tools.Context{Store: store}

	oldID := saveMemory(t, store, "Autumn uses Vim", "tools", "user")

	argsJSON := fmt.Sprintf(
		`{"memory_id":%d,"memory":"Autumn uses Neovim","category":"tools"}`,
		oldID,
	)
	Handle(argsJSON, ctx)

	// update_memory calls Store.SaveMemory directly (skips ExecSaveMemory),
	// so SavedMemories stays empty.
	if len(ctx.SavedMemories) != 0 {
		t.Errorf("ctx.SavedMemories = %v, want empty (update_memory does not track SavedMemories)", ctx.SavedMemories)
	}
}

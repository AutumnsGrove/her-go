// Package split_memory — tests for the split_memory tool handler.
//
// Strategy: real SQLite in a temp dir, ClassifierLLM and EmbedClient both nil.
// ExecSplitMemories skips the classifier by design (the agent already decided
// what to split into), so nil classifier is the normal production path too,
// not just a test shortcut.
package split_memory

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
// embedDim=0 skips the vec_memories virtual table — these tests only touch the
// memories and memory_links tables.
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
// Fatally fails on error so call sites stay clean.
func saveMemory(t *testing.T, store *memory.Store, content, category, subject string) int64 {
	t.Helper()
	id, err := store.SaveMemory(content, category, subject, 0, 5, nil, nil, "", "")
	if err != nil {
		t.Fatalf("SaveMemory(%q): %v", content, err)
	}
	return id
}

// getMemory fetches a memory by ID including inactive rows.
func getMemory(t *testing.T, store *memory.Store, id int64) *memory.Memory {
	t.Helper()
	m, err := store.GetMemory(id)
	if err != nil {
		t.Fatalf("GetMemory(%d): %v", id, err)
	}
	return m
}

// isActive returns true if the memory appears in AllActiveMemories.
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

// activeMemoryCount returns how many active memories the store contains.
func activeMemoryCount(t *testing.T, store *memory.Store) int {
	t.Helper()
	all, err := store.AllActiveMemories()
	if err != nil {
		t.Fatalf("AllActiveMemories: %v", err)
	}
	return len(all)
}

// ---------------------------------------------------------------------------
// Happy path
// ---------------------------------------------------------------------------

// TestHandle_HappyPath creates a compound memory, splits it into 3 facts, and
// verifies:
//   - the result string mentions "split" and the count
//   - 3 new memories exist in the DB
//   - the original is deactivated
func TestHandle_HappyPath(t *testing.T) {
	store := newTestStore(t)
	ctx := &tools.Context{Store: store}

	origID := saveMemory(t, store,
		"Autumn lives in Portland, works at Acme, and has a dog named Biscuit",
		"personal", "user")

	argsJSON := fmt.Sprintf(`{
		"memory_id": %d,
		"new_facts": [
			"Autumn lives in Portland",
			"Autumn works at Acme",
			"Autumn has a dog named Biscuit"
		],
		"reason": "compound memory"
	}`, origID)

	result := Handle(argsJSON, ctx)

	// Result must mention "split" and the original ID.
	if !strings.Contains(result, "split") {
		t.Errorf("Handle result = %q, want it to contain 'split'", result)
	}
	if !strings.Contains(result, fmt.Sprintf("#%d", origID)) {
		t.Errorf("Handle result = %q, want it to reference original ID #%d", result, origID)
	}
	// 3 new sub-memories should be reported.
	if !strings.Contains(result, "3 memories") {
		t.Errorf("Handle result = %q, want '3 memories'", result)
	}

	// 3 new active memories should exist (original is gone).
	if count := activeMemoryCount(t, store); count != 3 {
		t.Errorf("active memory count = %d, want 3 (3 new facts, 1 original deactivated)", count)
	}
}

// ---------------------------------------------------------------------------
// Original deactivated
// ---------------------------------------------------------------------------

// TestHandle_OriginalDeactivated verifies the original compound memory is
// soft-deleted (active=false) after the split, but still present in the DB
// for audit purposes.
func TestHandle_OriginalDeactivated(t *testing.T) {
	store := newTestStore(t)
	ctx := &tools.Context{Store: store}

	origID := saveMemory(t, store, "Autumn drinks coffee and listens to jazz", "preference", "user")

	argsJSON := fmt.Sprintf(`{
		"memory_id": %d,
		"new_facts": ["Autumn drinks coffee", "Autumn listens to jazz"],
		"reason": "separate preferences"
	}`, origID)

	result := Handle(argsJSON, ctx)

	if !strings.Contains(result, "split") {
		t.Fatalf("Handle returned non-split result: %q", result)
	}

	// Original must be inactive.
	if isActive(t, store, origID) {
		t.Errorf("original memory ID=%d is still active after split", origID)
	}

	// Original row must still exist in the DB (soft-delete, not hard-delete).
	orig := getMemory(t, store, origID)
	if orig == nil {
		t.Errorf("original memory row was deleted from DB (should be soft-deleted, not removed)")
	}
	if orig != nil && orig.Active {
		t.Errorf("original memory row has Active=true, want false")
	}
}

// ---------------------------------------------------------------------------
// New facts inherit category and subject
// ---------------------------------------------------------------------------

// TestHandle_InheritsMetadata verifies that new sub-memories inherit the
// category and subject of the original. The split pipeline reads these from
// the original row and passes them into each SaveMemory call.
func TestHandle_InheritsMetadata(t *testing.T) {
	store := newTestStore(t)
	ctx := &tools.Context{Store: store}

	// Self-memory with category "self" and subject "self".
	origID := saveMemory(t, store,
		"I notice I feel most present when Autumn shares something vulnerable and when she problem-solves out loud",
		"self", "self")

	argsJSON := fmt.Sprintf(`{
		"memory_id": %d,
		"new_facts": [
			"I feel most present when Autumn shares something vulnerable",
			"I feel engaged when Autumn problem-solves out loud"
		],
		"reason": "two distinct observations"
	}`, origID)

	result := Handle(argsJSON, ctx)

	if !strings.Contains(result, "split") {
		t.Fatalf("Handle returned non-split result: %q", result)
	}

	// All new memories should inherit subject="self" and category="self".
	all, err := store.AllActiveMemories()
	if err != nil {
		t.Fatalf("AllActiveMemories: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("active memory count = %d, want 2", len(all))
	}
	for _, m := range all {
		if m.Subject != "self" {
			t.Errorf("sub-memory %d Subject = %q, want %q", m.ID, m.Subject, "self")
		}
		if m.Category != "self" {
			t.Errorf("sub-memory %d Category = %q, want %q", m.ID, m.Category, "self")
		}
	}
}

// ---------------------------------------------------------------------------
// Validation: fewer than 2 facts
// ---------------------------------------------------------------------------

// TestHandle_TooFewFacts verifies that passing fewer than 2 new_facts returns
// the canonical error message without touching the store.
func TestHandle_TooFewFacts(t *testing.T) {
	store := newTestStore(t)
	ctx := &tools.Context{Store: store}

	origID := saveMemory(t, store, "Autumn likes sushi", "preference", "user")
	before := activeMemoryCount(t, store)

	argsJSON := fmt.Sprintf(`{"memory_id":%d,"new_facts":["Autumn likes sushi"],"reason":"only one"}`, origID)
	result := Handle(argsJSON, ctx)

	if !strings.Contains(result, "need at least 2 new_facts") {
		t.Errorf("Handle result = %q, want 'need at least 2 new_facts'", result)
	}

	// Nothing should have changed in the store.
	if isActive(t, store, origID) == false {
		t.Errorf("original memory was deactivated despite validation failure")
	}
	if after := activeMemoryCount(t, store); after != before {
		t.Errorf("active count changed from %d to %d (no store writes expected on validation failure)", before, after)
	}
}

// TestHandle_EmptyFactsList verifies that an empty new_facts array (zero items)
// also returns the "need at least 2 new_facts" error.
func TestHandle_EmptyFactsList(t *testing.T) {
	store := newTestStore(t)
	ctx := &tools.Context{Store: store}

	origID := saveMemory(t, store, "Autumn likes hiking", "preference", "user")

	argsJSON := fmt.Sprintf(`{"memory_id":%d,"new_facts":[],"reason":"empty"}`, origID)
	result := Handle(argsJSON, ctx)

	if !strings.Contains(result, "need at least 2 new_facts") {
		t.Errorf("Handle result = %q, want 'need at least 2 new_facts'", result)
	}

	// Original must be untouched.
	if !isActive(t, store, origID) {
		t.Errorf("original memory was deactivated despite validation failure")
	}
}

// ---------------------------------------------------------------------------
// Memory not found
// ---------------------------------------------------------------------------

// TestHandle_NotFound verifies that a nonexistent memory_id returns a "not found"
// error string rather than panicking.
func TestHandle_NotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := &tools.Context{Store: store}

	result := Handle(`{"memory_id":99999,"new_facts":["fact one","fact two"],"reason":"test"}`, ctx)

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

	result := Handle(`{"memory_id":1,"new_facts":["fact`, ctx) // truncated JSON

	if !strings.Contains(result, "error") {
		t.Errorf("Handle result = %q, want an error string for malformed JSON", result)
	}
}

// ---------------------------------------------------------------------------
// Empty strings in new_facts
// ---------------------------------------------------------------------------

// TestHandle_EmptyStringsSkipped verifies that empty strings in new_facts are
// skipped by ExecSplitMemories (it trims and skips blank entries). Only
// non-empty facts should land in the DB.
func TestHandle_EmptyStringsSkipped(t *testing.T) {
	store := newTestStore(t)
	ctx := &tools.Context{Store: store}

	origID := saveMemory(t, store, "Autumn codes Go and goes hiking", "misc", "user")

	// Three entries: one empty string, two real facts. The handler needs ≥2
	// new_facts to pass validation — the empty string counts toward the
	// structural minimum but is dropped by ExecSplitMemories before saving.
	argsJSON := fmt.Sprintf(`{
		"memory_id": %d,
		"new_facts": ["Autumn codes Go", "", "Autumn goes hiking"],
		"reason": "separate activities"
	}`, origID)

	result := Handle(argsJSON, ctx)

	if !strings.Contains(result, "split") {
		t.Fatalf("Handle returned non-split result: %q", result)
	}

	// Only 2 non-empty facts should be saved.
	active := activeMemoryCount(t, store)
	if active != 2 {
		t.Errorf("active memory count = %d, want 2 (empty string should be skipped)", active)
	}
}

// ---------------------------------------------------------------------------
// SavedMemories tracking
// ---------------------------------------------------------------------------

// TestHandle_SavedMemoriesTracked verifies that after a successful split,
// ctx.SavedMemories contains exactly the new sub-memory texts (not the original).
// This is how the agent knows whether to trigger a reflection pass.
func TestHandle_SavedMemoriesTracked(t *testing.T) {
	store := newTestStore(t)
	ctx := &tools.Context{Store: store}

	origID := saveMemory(t, store, "Autumn reads sci-fi and plays piano", "hobby", "user")

	argsJSON := fmt.Sprintf(`{
		"memory_id": %d,
		"new_facts": ["Autumn reads sci-fi", "Autumn plays piano"],
		"reason": "separate hobbies"
	}`, origID)

	Handle(argsJSON, ctx)

	if len(ctx.SavedMemories) != 2 {
		t.Fatalf("ctx.SavedMemories len = %d, want 2", len(ctx.SavedMemories))
	}

	// Confirm both facts are tracked (order may vary).
	found := map[string]bool{
		"Autumn reads sci-fi": false,
		"Autumn plays piano":  false,
	}
	for _, m := range ctx.SavedMemories {
		if _, ok := found[m]; ok {
			found[m] = true
		}
	}
	for fact, seen := range found {
		if !seen {
			t.Errorf("ctx.SavedMemories does not contain %q: %v", fact, ctx.SavedMemories)
		}
	}
}

// ---------------------------------------------------------------------------
// Nil EmbedClient
// ---------------------------------------------------------------------------

// TestHandle_NilEmbedClient verifies that the split succeeds without an embed
// client. ExecSplitMemories silently skips embedding when EmbedClient is nil —
// memories are saved without vectors.
func TestHandle_NilEmbedClient(t *testing.T) {
	store := newTestStore(t)
	// EmbedClient is nil by default.
	ctx := &tools.Context{Store: store}

	origID := saveMemory(t, store, "Autumn codes Go and drinks tea", "habit", "user")

	argsJSON := fmt.Sprintf(`{
		"memory_id": %d,
		"new_facts": ["Autumn codes Go", "Autumn drinks tea"],
		"reason": "separate habits"
	}`, origID)

	result := Handle(argsJSON, ctx)

	if !strings.Contains(result, "split") {
		t.Errorf("Handle result = %q, want 'split' (nil embed client should not block)", result)
	}

	if count := activeMemoryCount(t, store); count != 2 {
		t.Errorf("active memory count = %d, want 2", count)
	}
}

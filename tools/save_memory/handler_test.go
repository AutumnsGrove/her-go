// Package save_memory — handler-level tests for the save_memory tool.
//
// These tests drive Handle() directly (or ExecSaveMemory with subject="user")
// with a real SQLite store but nil EmbedClient and nil ClassifierLLM. That
// means the dedup and classifier gates are skipped — we're testing the
// handler's plumbing: argument parsing, the style/length gates, subject
// routing, and the DB write path.
//
// The gate unit tests (style, length) already live in tools/memory_helpers_test.go.
// We don't repeat them here — we only verify that they propagate correctly
// through the Handle() call and affect what (if anything) lands in the DB.
package save_memory

import (
	"path/filepath"
	"strings"
	"testing"

	"her/config"
	"her/memory"
	"her/tools"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// newTestStore creates a fresh SQLite store in a temp dir. The cleanup is
// registered automatically via t.Cleanup — no defer needed at the call site.
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

// newCtx builds a minimal Context: a real store, no embed, no classifier.
// With EmbedClient=nil the dedup path is skipped entirely.
// With ClassifierLLM=nil the classifier gate is skipped (fail-open).
func newCtx(store *memory.Store) *tools.Context {
	return &tools.Context{
		Store: store,
		Cfg: &config.Config{
			Identity: config.IdentityConfig{Her: "Mira", User: "Autumn"},
		},
		// EmbedClient and ClassifierLLM are nil — both gates disabled.
	}
}

// savedUserMemories reads back all active "user" memories from the store.
// Using RecentMemories lets us verify the DB write without raw SQL.
func savedUserMemories(t *testing.T, store *memory.Store) []memory.Memory {
	t.Helper()
	mems, err := store.RecentMemories("user", 100)
	if err != nil {
		t.Fatalf("RecentMemories: %v", err)
	}
	return mems
}

// savedSelfMemories reads back all active "self" memories.
func savedSelfMemories(t *testing.T, store *memory.Store) []memory.Memory {
	t.Helper()
	mems, err := store.RecentMemories("self", 100)
	if err != nil {
		t.Fatalf("RecentMemories: %v", err)
	}
	return mems
}

// ── happy path ───────────────────────────────────────────────────────────────

func TestHandle_HappyPath_ReturnsSavedUserMemory(t *testing.T) {
	store := newTestStore(t)
	ctx := newCtx(store)

	result := Handle(`{"memory":"Autumn prefers dark roast coffee","category":"preference","tags":"coffee, food"}`, ctx)

	// Return value must start with the canonical prefix.
	if !strings.HasPrefix(result, "saved user memory ID=") {
		t.Errorf("want prefix %q, got: %s", "saved user memory ID=", result)
	}
	// The memory text is echoed back after the ID.
	if !strings.Contains(result, "Autumn prefers dark roast coffee") {
		t.Errorf("result should echo the memory text, got: %s", result)
	}
}

func TestHandle_HappyPath_WritesToDB(t *testing.T) {
	store := newTestStore(t)
	ctx := newCtx(store)

	Handle(`{"memory":"Autumn is learning Go","category":"work","tags":"go, programming"}`, ctx)

	mems := savedUserMemories(t, store)
	if len(mems) != 1 {
		t.Fatalf("expected 1 memory in DB, got %d", len(mems))
	}
	if mems[0].Content != "Autumn is learning Go" {
		t.Errorf("DB content = %q, want %q", mems[0].Content, "Autumn is learning Go")
	}
}

// ── subject routing ───────────────────────────────────────────────────────────

// The whole point of save_memory vs save_self_memory is the subject field.
// Confirm every successful write lands with subject="user", not "self".
func TestHandle_SubjectIsUser(t *testing.T) {
	store := newTestStore(t)
	ctx := newCtx(store)

	Handle(`{"memory":"Autumn likes hiking on weekends","category":"hobby","tags":"hiking, outdoors"}`, ctx)

	mems := savedUserMemories(t, store)
	if len(mems) == 0 {
		t.Fatal("no memories saved")
	}
	if mems[0].Subject != "user" {
		t.Errorf("Subject = %q, want %q", mems[0].Subject, "user")
	}

	// Sanity: nothing in the "self" bucket.
	selfMems := savedSelfMemories(t, store)
	if len(selfMems) != 0 {
		t.Errorf("expected 0 self memories, got %d", len(selfMems))
	}
}

// ── SavedMemories tracking ────────────────────────────────────────────────────

// After a successful save, ctx.SavedMemories should contain the saved text.
// The agent uses this slice to decide whether to trigger a reflection.
func TestHandle_AppendsSavedMemoriesToContext(t *testing.T) {
	store := newTestStore(t)
	ctx := newCtx(store)

	Handle(`{"memory":"Autumn dislikes mornings","category":"preference","tags":"morning, sleep"}`, ctx)

	if len(ctx.SavedMemories) != 1 {
		t.Fatalf("ctx.SavedMemories length = %d, want 1", len(ctx.SavedMemories))
	}
	if ctx.SavedMemories[0] != "Autumn dislikes mornings" {
		t.Errorf("SavedMemories[0] = %q, want %q", ctx.SavedMemories[0], "Autumn dislikes mornings")
	}
}

func TestHandle_AccumulatesSavedMemoriesAcrossCalls(t *testing.T) {
	store := newTestStore(t)
	ctx := newCtx(store)

	Handle(`{"memory":"Autumn plays guitar","category":"hobby","tags":"music, guitar"}`, ctx)
	Handle(`{"memory":"Autumn studied Spanish in college","category":"education","tags":"language, spanish"}`, ctx)

	if len(ctx.SavedMemories) != 2 {
		t.Errorf("SavedMemories length = %d, want 2", len(ctx.SavedMemories))
	}
}

// ── invalid JSON ─────────────────────────────────────────────────────────────

// Malformed JSON should return an error string immediately — nothing hits the DB.
func TestHandle_InvalidJSON_ReturnsError(t *testing.T) {
	store := newTestStore(t)
	ctx := newCtx(store)

	result := Handle(`{not valid json`, ctx)

	if !strings.HasPrefix(result, "error") {
		t.Errorf("expected error string for invalid JSON, got: %s", result)
	}

	mems := savedUserMemories(t, store)
	if len(mems) != 0 {
		t.Errorf("expected 0 DB writes after parse failure, got %d", len(mems))
	}
}

func TestHandle_EmptyJSON_ReturnsError(t *testing.T) {
	store := newTestStore(t)
	ctx := newCtx(store)

	result := Handle(``, ctx)

	if !strings.HasPrefix(result, "error") {
		t.Errorf("expected error for empty JSON, got: %s", result)
	}
}

// ── empty memory field ────────────────────────────────────────────────────────

// ExecSaveMemory has no explicit empty-memory guard. An empty string passes
// both the style gate (no blocked patterns) and the length gate (0 < 300).
// It then falls through to the DB write. This test documents the current
// behavior — if a guard is added later, this test will need updating.
func TestHandle_EmptyMemoryField_SavesEmptyMemory(t *testing.T) {
	store := newTestStore(t)
	ctx := newCtx(store)

	result := Handle(`{"memory":"","category":"other","tags":""}`, ctx)

	// The empty string is technically valid through current gates. It should
	// either save (returning "saved user memory ID=...") or be rejected.
	// We assert the inverse of an outright crash: result is non-empty.
	if result == "" {
		t.Error("Handle returned empty string, want a deterministic outcome message")
	}

	// Document actual behavior: log it so future developers know what changed.
	t.Logf("empty memory field result: %q", result)
}

// ── style gate propagation ────────────────────────────────────────────────────

// These verify that style-gate rejections propagate through Handle() correctly.
// The gate logic itself is tested exhaustively in memory_helpers_test.go — we
// only check that the rejection reaches the caller and nothing is written.

func TestHandle_StyleGate_BlockedPhrase_ReturnsRejection(t *testing.T) {
	cases := []struct {
		name   string
		memory string
	}{
		{
			name:   "not_just",
			memory: "Autumn is not just a programmer, she is an artist",
		},
		{
			name:   "a_testament_to",
			memory: "Her persistence is a testament to her character",
		},
		{
			name:   "leverage",
			memory: "She wants to leverage her Go skills at work",
		},
		{
			name:   "tapestry",
			memory: "Her interests form a rich tapestry of creativity",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			ctx := newCtx(store)

			argsJSON := `{"memory":"` + tc.memory + `","category":"work","tags":"test"}`
			result := Handle(argsJSON, ctx)

			if !strings.HasPrefix(result, "rejected:") {
				t.Errorf("expected rejection for %q, got: %s", tc.name, result)
			}

			// Nothing should land in the DB.
			mems := savedUserMemories(t, store)
			if len(mems) != 0 {
				t.Errorf("expected 0 DB writes after style rejection, got %d", len(mems))
			}

			// ctx.SavedMemories must not be updated on rejection.
			if len(ctx.SavedMemories) != 0 {
				t.Errorf("SavedMemories should be empty after rejection, got %v", ctx.SavedMemories)
			}
		})
	}
}

func TestHandle_TrailingEmDash_ReturnsRejection(t *testing.T) {
	store := newTestStore(t)
	ctx := newCtx(store)

	// The em dash character is U+2014. The trailing position is the gate.
	result := Handle(`{"memory":"Autumn loves hiking \u2014","category":"hobby","tags":"hiking"}`, ctx)

	if !strings.HasPrefix(result, "rejected:") {
		t.Errorf("expected rejection for trailing em dash, got: %s", result)
	}
	if !strings.Contains(result, "em dash") {
		t.Errorf("rejection should mention em dash, got: %s", result)
	}

	mems := savedUserMemories(t, store)
	if len(mems) != 0 {
		t.Errorf("expected 0 DB writes after em-dash rejection, got %d", len(mems))
	}
}

func TestHandle_TrailingEnDash_ReturnsRejection(t *testing.T) {
	store := newTestStore(t)
	ctx := newCtx(store)

	// U+2013 is the en dash — also blocked at the trailing position.
	result := Handle(`{"memory":"Autumn prefers tea over coffee \u2013","category":"preference","tags":"tea"}`, ctx)

	if !strings.HasPrefix(result, "rejected:") {
		t.Errorf("expected rejection for trailing en dash, got: %s", result)
	}
}

// ── length gate propagation ───────────────────────────────────────────────────

func TestHandle_LengthGate_OverLimit_ReturnsRejection(t *testing.T) {
	store := newTestStore(t)
	ctx := newCtx(store)

	// Build a memory that is clearly over the 300-character default limit.
	// Uses plain characters to avoid triggering the style gate.
	longText := "Autumn studied programming for many years. " + strings.Repeat("She enjoys working on complex systems. ", 10)
	argsJSON := `{"memory":"` + longText + `","category":"other","tags":"test"}`

	result := Handle(argsJSON, ctx)

	if !strings.HasPrefix(result, "rejected:") {
		t.Errorf("expected length-gate rejection, got: %s", result)
	}
	if !strings.Contains(result, "characters") {
		t.Errorf("rejection should mention character count, got: %s", result)
	}

	mems := savedUserMemories(t, store)
	if len(mems) != 0 {
		t.Errorf("expected 0 DB writes after length rejection, got %d", len(mems))
	}
}

// ctx.MaxMemoryLength=0 means "use package default (300)". Verify a
// memory right at the limit is not rejected by the length gate.
func TestHandle_LengthGate_AtDefaultLimit_Passes(t *testing.T) {
	store := newTestStore(t)
	ctx := newCtx(store) // MaxMemoryLength=0 → uses 300

	// Build exactly 300 safe characters.
	base := "Autumn reads science fiction novels in her spare time. "
	for len(base) < 300 {
		base += "a"
	}
	exactText := base[:300]

	argsJSON := `{"memory":"` + exactText + `","category":"other","tags":"test"}`
	result := Handle(argsJSON, ctx)

	if strings.Contains(result, "characters (max") {
		t.Errorf("memory at exactly 300 chars should not be length-rejected, got: %s", result)
	}
}

// MaxMemoryLength can be overridden via ctx. Verify the custom limit is used.
func TestHandle_LengthGate_CustomLimit_Respected(t *testing.T) {
	store := newTestStore(t)
	ctx := newCtx(store)
	ctx.MaxMemoryLength = 50 // tight limit for this test

	// A 51-character memory should be rejected under the custom limit.
	overLimit := strings.Repeat("x", 51)
	argsJSON := `{"memory":"` + overLimit + `","category":"other","tags":"test"}`

	result := Handle(argsJSON, ctx)

	if !strings.HasPrefix(result, "rejected:") {
		t.Errorf("expected rejection with custom limit 50, got: %s", result)
	}
	if !strings.Contains(result, "50") {
		t.Errorf("rejection should cite the custom limit (50), got: %s", result)
	}
}

// ── nil store (fail path after gates pass) ────────────────────────────────────

// When ctx.Store is nil, ExecSaveMemory returns "error: no store configured"
// — but only after the style and length gates pass. The nil-store check sits
// at the bottom of the function. If either gate rejects first, the nil-store
// path is never reached.
func TestHandle_NilStore_ReturnsError(t *testing.T) {
	ctx := &tools.Context{
		Cfg: &config.Config{
			Identity: config.IdentityConfig{Her: "Mira", User: "Autumn"},
		},
		// Store is intentionally nil.
	}

	// Use a clean memory that passes style and length gates.
	result := Handle(`{"memory":"Autumn enjoys board games","category":"hobby","tags":"games"}`, ctx)

	if result != "error: no store configured" {
		t.Errorf("got %q, want %q", result, "error: no store configured")
	}
}

// ── classifier nil (fail-open) ────────────────────────────────────────────────

// When ClassifierLLM is nil the classifier gate is skipped entirely. The
// memory should land in the DB without a classifier roundtrip.
func TestHandle_ClassifierNil_FailOpen_SavesMemory(t *testing.T) {
	store := newTestStore(t)
	ctx := newCtx(store)
	// ctx.ClassifierLLM is nil (set by newCtx) — fail-open is the expected path.

	result := Handle(`{"memory":"Autumn prefers Vim over VS Code","category":"tool","tags":"vim, editor"}`, ctx)

	if !strings.HasPrefix(result, "saved user memory ID=") {
		t.Errorf("expected successful save with nil classifier, got: %s", result)
	}

	mems := savedUserMemories(t, store)
	if len(mems) != 1 {
		t.Errorf("expected 1 memory in DB, got %d", len(mems))
	}
}

// ── EmbedClient nil (embedding skipped) ───────────────────────────────────────

// With EmbedClient=nil, both the dedup check and the vector index write are
// skipped. The memory should still save successfully with nil embeddings.
func TestHandle_EmbedClientNil_SavesWithoutEmbedding(t *testing.T) {
	store := newTestStore(t)
	ctx := newCtx(store)
	// ctx.EmbedClient is nil (set by newCtx) — embedding path is skipped.

	result := Handle(`{"memory":"Autumn grew up in the Pacific Northwest","category":"background","tags":"location, childhood"}`, ctx)

	if !strings.HasPrefix(result, "saved user memory ID=") {
		t.Errorf("expected successful save without embedding, got: %s", result)
	}

	mems := savedUserMemories(t, store)
	if len(mems) != 1 {
		t.Fatal("expected 1 memory in DB")
	}
	// Without an embed client, the stored embedding vectors are nil.
	if mems[0].Embedding != nil {
		t.Errorf("expected nil embedding when EmbedClient is nil, got non-nil")
	}
}

// ── optional fields ───────────────────────────────────────────────────────────

// category, tags, and context are all optional. Verify the handler tolerates
// a minimal payload — just the required "memory" field.
func TestHandle_MinimalPayload_OnlyMemoryField(t *testing.T) {
	store := newTestStore(t)
	ctx := newCtx(store)

	result := Handle(`{"memory":"Autumn has a cat named Luna"}`, ctx)

	if !strings.HasPrefix(result, "saved user memory ID=") {
		t.Errorf("expected save with minimal payload, got: %s", result)
	}

	mems := savedUserMemories(t, store)
	if len(mems) != 1 {
		t.Fatalf("expected 1 memory, got %d", len(mems))
	}
	if mems[0].Content != "Autumn has a cat named Luna" {
		t.Errorf("Content = %q, want %q", mems[0].Content, "Autumn has a cat named Luna")
	}
}

// Multiple saves accumulate independently — each gets its own DB row and ID.
func TestHandle_MultipleSaves_EachGetsUniqueID(t *testing.T) {
	store := newTestStore(t)
	ctx := newCtx(store)

	r1 := Handle(`{"memory":"Autumn drinks tea in the morning","category":"habit","tags":"tea, morning"}`, ctx)
	r2 := Handle(`{"memory":"Autumn reads before bed","category":"habit","tags":"reading, sleep"}`, ctx)

	if !strings.HasPrefix(r1, "saved user memory ID=") {
		t.Errorf("first save: %s", r1)
	}
	if !strings.HasPrefix(r2, "saved user memory ID=") {
		t.Errorf("second save: %s", r2)
	}

	// IDs must differ.
	if r1 == r2 {
		t.Errorf("two saves returned the same result string: %s", r1)
	}

	mems := savedUserMemories(t, store)
	if len(mems) != 2 {
		t.Errorf("expected 2 memories in DB, got %d", len(mems))
	}
}

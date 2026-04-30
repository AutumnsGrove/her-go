// Package save_self_memory — handler-level tests for the save_self_memory tool.
//
// save_self_memory is identical to save_memory in every way except for the
// subject field it passes to ExecSaveMemory ("self" instead of "user"). So
// these tests are deliberately narrower — they verify the subject routing and
// the success path. Full gate coverage lives in:
//
//   - tools/memory_helpers_test.go  (style gate, length gate — source of truth)
//   - tools/save_memory/handler_test.go  (full gate propagation at handler level)
//
// We do not duplicate those test cases here.
package save_self_memory

import (
	"path/filepath"
	"strings"
	"testing"

	"her/config"
	"her/memory"
	"her/tools"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func newTestStore(t *testing.T) memory.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := memory.NewStore(dbPath, 0)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// newCtx returns a Context with a real store but nil EmbedClient and nil
// ClassifierLLM — both dedup and classifier gates are skipped.
func newCtx(store memory.Store) *tools.Context {
	return &tools.Context{
		Store: store,
		Cfg: &config.Config{
			Identity: config.IdentityConfig{Her: "Mira", User: "Autumn"},
		},
	}
}

// ── happy path ────────────────────────────────────────────────────────────────

func TestHandle_HappyPath_ReturnsSavedSelfMemory(t *testing.T) {
	store := newTestStore(t)
	ctx := newCtx(store)

	result := Handle(`{"memory":"I notice I respond better when I pause before answering","category":"pattern","tags":"communication, introspection"}`, ctx)

	if !strings.HasPrefix(result, "saved self memory ID=") {
		t.Errorf("want prefix %q, got: %s", "saved self memory ID=", result)
	}
	if !strings.Contains(result, "I notice I respond better when I pause before answering") {
		t.Errorf("result should echo the memory text, got: %s", result)
	}
}

// ── subject routing ───────────────────────────────────────────────────────────

// The critical contract: every write from save_self_memory must land with
// subject="self". Verify by reading back from the store using RecentMemories.
func TestHandle_SubjectIsSelf(t *testing.T) {
	store := newTestStore(t)
	ctx := newCtx(store)

	Handle(`{"memory":"I tend to over-explain when I am uncertain","category":"pattern","tags":"communication, uncertainty"}`, ctx)

	selfMems, err := store.RecentMemories("self", 10)
	if err != nil {
		t.Fatalf("RecentMemories: %v", err)
	}
	if len(selfMems) != 1 {
		t.Fatalf("expected 1 self memory, got %d", len(selfMems))
	}
	if selfMems[0].Subject != "self" {
		t.Errorf("Subject = %q, want %q", selfMems[0].Subject, "self")
	}

	// Nothing should land in the "user" bucket.
	userMems, err := store.RecentMemories("user", 10)
	if err != nil {
		t.Fatalf("RecentMemories user: %v", err)
	}
	if len(userMems) != 0 {
		t.Errorf("expected 0 user memories, got %d", len(userMems))
	}
}

// Verify the memory text itself is stored correctly — not just the subject.
func TestHandle_SubjectIsSelf_ContentRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := newCtx(store)

	const memText = "I find it easier to be direct when Autumn uses short sentences"
	Handle(`{"memory":"`+memText+`","category":"observation","tags":"communication"}`, ctx)

	selfMems, err := store.RecentMemories("self", 10)
	if err != nil {
		t.Fatalf("RecentMemories: %v", err)
	}
	if len(selfMems) == 0 {
		t.Fatal("no self memories saved")
	}
	if selfMems[0].Content != memText {
		t.Errorf("Content = %q, want %q", selfMems[0].Content, memText)
	}
}

// ── SavedMemories tracking ────────────────────────────────────────────────────

// Same tracking semantics as save_memory — ctx.SavedMemories grows on success.
func TestHandle_AppendsSavedMemoriesToContext(t *testing.T) {
	store := newTestStore(t)
	ctx := newCtx(store)

	Handle(`{"memory":"I work best when I have clear objectives","category":"pattern","tags":"clarity, focus"}`, ctx)

	if len(ctx.SavedMemories) != 1 {
		t.Fatalf("ctx.SavedMemories length = %d, want 1", len(ctx.SavedMemories))
	}
	if ctx.SavedMemories[0] != "I work best when I have clear objectives" {
		t.Errorf("SavedMemories[0] = %q", ctx.SavedMemories[0])
	}
}

// ── gate propagation (smoke-level, not exhaustive) ────────────────────────────

// We verify that the style gate still fires for self-memories, and that on
// rejection nothing lands in the DB and SavedMemories is unchanged.
// Full gate tests are in memory_helpers_test.go — don't duplicate them.

func TestHandle_StyleGate_StillApplies_ToSelfMemories(t *testing.T) {
	store := newTestStore(t)
	ctx := newCtx(store)

	// "tapestry" is on the blocklist.
	result := Handle(`{"memory":"My communication style forms a rich tapestry of care","category":"pattern","tags":"communication"}`, ctx)

	if !strings.HasPrefix(result, "rejected:") {
		t.Errorf("expected style-gate rejection for self memory, got: %s", result)
	}

	selfMems, err := store.RecentMemories("self", 10)
	if err != nil {
		t.Fatalf("RecentMemories: %v", err)
	}
	if len(selfMems) != 0 {
		t.Errorf("expected 0 DB writes after style rejection, got %d", len(selfMems))
	}
	if len(ctx.SavedMemories) != 0 {
		t.Errorf("SavedMemories should be empty after rejection, got %v", ctx.SavedMemories)
	}
}

func TestHandle_InvalidJSON_ReturnsError_NothingSaved(t *testing.T) {
	store := newTestStore(t)
	ctx := newCtx(store)

	result := Handle(`not json at all`, ctx)

	if !strings.HasPrefix(result, "error") {
		t.Errorf("expected error for invalid JSON, got: %s", result)
	}

	selfMems, _ := store.RecentMemories("self", 10)
	if len(selfMems) != 0 {
		t.Errorf("expected 0 DB writes, got %d", len(selfMems))
	}
}

// ── nil store ─────────────────────────────────────────────────────────────────

func TestHandle_NilStore_ReturnsError(t *testing.T) {
	ctx := &tools.Context{
		Cfg: &config.Config{
			Identity: config.IdentityConfig{Her: "Mira", User: "Autumn"},
		},
		// Store intentionally nil.
	}

	result := Handle(`{"memory":"I am curious about the world","category":"identity","tags":"curiosity"}`, ctx)

	if result != "error: no store configured" {
		t.Errorf("got %q, want %q", result, "error: no store configured")
	}
}

// ── isolation between subjects ────────────────────────────────────────────────

// When both save_memory and save_self_memory are used in the same turn, the
// writes must land in separate subject buckets with no cross-contamination.
// This test calls ExecSaveMemory directly to simulate both handlers sharing
// one Context (as they do in the real agent loop).
func TestHandle_SelfAndUserMemories_AreIsolated(t *testing.T) {
	store := newTestStore(t)
	ctx := newCtx(store)

	// Simulate save_memory writing a user memory first.
	tools.ExecSaveMemory(
		`{"memory":"Autumn owns a dog named Max","category":"background","tags":"dog, pets"}`,
		"user",
		ctx,
	)
	// Then save_self_memory writing a self memory.
	Handle(`{"memory":"I remember to ask follow-up questions about her pets","category":"pattern","tags":"pets, conversation"}`, ctx)

	userMems, err := store.RecentMemories("user", 10)
	if err != nil {
		t.Fatalf("RecentMemories user: %v", err)
	}
	selfMems, err := store.RecentMemories("self", 10)
	if err != nil {
		t.Fatalf("RecentMemories self: %v", err)
	}

	if len(userMems) != 1 {
		t.Errorf("user memories = %d, want 1", len(userMems))
	}
	if len(selfMems) != 1 {
		t.Errorf("self memories = %d, want 1", len(selfMems))
	}

	if userMems[0].Subject != "user" {
		t.Errorf("user memory Subject = %q, want %q", userMems[0].Subject, "user")
	}
	if selfMems[0].Subject != "self" {
		t.Errorf("self memory Subject = %q, want %q", selfMems[0].Subject, "self")
	}
}

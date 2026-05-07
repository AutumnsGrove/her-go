package merge_memories

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"her/config"
	"her/memory"
	"her/tools"
)

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

func saveMemory(t *testing.T, store memory.Store, content string) int64 {
	t.Helper()
	id, err := store.SaveMemory(content, "test", "user", 0, 5, nil, nil, "", "")
	if err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}
	return id
}

func makeCtx(store memory.Store) *tools.Context {
	return &tools.Context{
		Store: store,
		Cfg:   &config.Config{},
	}
}

func TestMerge_Basic(t *testing.T) {
	store := newTestStore(t)
	id1 := saveMemory(t, store, "sobriety fact A")
	id2 := saveMemory(t, store, "sobriety fact B")

	args, _ := json.Marshal(map[string]any{
		"memory_ids":  []int64{id1, id2},
		"merged_text": "consolidated sobriety fact",
		"category":    "health",
		"reason":      "redundant",
	})

	result := Handle(string(args), makeCtx(store))

	if !strings.Contains(result, "merged 2 memories") {
		t.Errorf("unexpected result: %s", result)
	}

	// Source memories should be inactive.
	m1, _ := store.GetMemory(id1)
	if m1.Active {
		t.Error("source memory 1 should be inactive")
	}
	m2, _ := store.GetMemory(id2)
	if m2.Active {
		t.Error("source memory 2 should be inactive")
	}

	// Audit entry should exist.
	audits, _ := store.RecentDreamAudits(10)
	if len(audits) != 1 {
		t.Fatalf("expected 1 audit, got %d", len(audits))
	}
	if audits[0].Operation != "merge" {
		t.Errorf("audit operation: got %q, want %q", audits[0].Operation, "merge")
	}
}

func TestMerge_TooFewIDs(t *testing.T) {
	store := newTestStore(t)
	id1 := saveMemory(t, store, "only one")

	args, _ := json.Marshal(map[string]any{
		"memory_ids":  []int64{id1},
		"merged_text": "merged",
		"category":    "test",
		"reason":      "test",
	})

	result := Handle(string(args), makeCtx(store))
	if !strings.Contains(result, "at least 2") {
		t.Errorf("expected error about minimum IDs, got: %s", result)
	}
}

func TestMerge_EmptyText(t *testing.T) {
	args, _ := json.Marshal(map[string]any{
		"memory_ids":  []int64{1, 2},
		"merged_text": "",
		"category":    "test",
		"reason":      "test",
	})

	store := newTestStore(t)
	result := Handle(string(args), makeCtx(store))
	if !strings.Contains(result, "merged_text is required") {
		t.Errorf("expected error about empty text, got: %s", result)
	}
}

func TestMerge_NonexistentMemory(t *testing.T) {
	store := newTestStore(t)
	id1 := saveMemory(t, store, "real memory")

	args, _ := json.Marshal(map[string]any{
		"memory_ids":  []int64{id1, 9999},
		"merged_text": "merged",
		"category":    "test",
		"reason":      "test",
	})

	result := Handle(string(args), makeCtx(store))
	if !strings.Contains(result, "not found") {
		t.Errorf("expected not found error, got: %s", result)
	}
}

func TestMerge_DryRun(t *testing.T) {
	store := newTestStore(t)
	id1 := saveMemory(t, store, "fact A")
	id2 := saveMemory(t, store, "fact B")

	cfg := &config.Config{}
	dryRunTrue := true
	cfg.Dream.Enabled = &dryRunTrue
	cfg.Dream.DryRun = true

	ctx := &tools.Context{
		Store: store,
		Cfg:   cfg,
	}

	args, _ := json.Marshal(map[string]any{
		"memory_ids":  []int64{id1, id2},
		"merged_text": "merged fact",
		"category":    "test",
		"reason":      "test",
	})

	result := Handle(string(args), ctx)
	if !strings.Contains(result, "DRY RUN") {
		t.Errorf("expected dry run result, got: %s", result)
	}

	// Source memories should still be active.
	m1, _ := store.GetMemory(id1)
	if !m1.Active {
		t.Error("dry run should not deactivate source memory 1")
	}
}

package memory

import (
	"path/filepath"
	"testing"
)

func newTestStoreForDream(t *testing.T) *SQLiteStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := NewStore(dbPath, 0)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestSaveDreamAudit_RoundTrip(t *testing.T) {
	store := newTestStoreForDream(t)

	err := store.SaveDreamAudit("merge", []int64{1, 2, 3}, 10, "before text", "after text", "redundant cluster", false)
	if err != nil {
		t.Fatalf("SaveDreamAudit: %v", err)
	}

	audits, err := store.RecentDreamAudits(10)
	if err != nil {
		t.Fatalf("RecentDreamAudits: %v", err)
	}
	if len(audits) != 1 {
		t.Fatalf("expected 1 audit, got %d", len(audits))
	}

	a := audits[0]
	if a.Operation != "merge" {
		t.Errorf("operation: got %q, want %q", a.Operation, "merge")
	}
	if len(a.SourceIDs) != 3 || a.SourceIDs[0] != 1 || a.SourceIDs[1] != 2 || a.SourceIDs[2] != 3 {
		t.Errorf("source_ids: got %v, want [1 2 3]", a.SourceIDs)
	}
	if a.ResultID != 10 {
		t.Errorf("result_id: got %d, want 10", a.ResultID)
	}
	if a.BeforeText != "before text" {
		t.Errorf("before_text: got %q, want %q", a.BeforeText, "before text")
	}
	if a.AfterText != "after text" {
		t.Errorf("after_text: got %q, want %q", a.AfterText, "after text")
	}
	if a.Reason != "redundant cluster" {
		t.Errorf("reason: got %q, want %q", a.Reason, "redundant cluster")
	}
	if a.DryRun {
		t.Error("dry_run: got true, want false")
	}
}

func TestSaveDreamAudit_DryRun(t *testing.T) {
	store := newTestStoreForDream(t)

	err := store.SaveDreamAudit("expire", []int64{5}, 0, "", "expired", "stale mood", true)
	if err != nil {
		t.Fatalf("SaveDreamAudit: %v", err)
	}

	audits, err := store.RecentDreamAudits(10)
	if err != nil {
		t.Fatalf("RecentDreamAudits: %v", err)
	}
	if len(audits) != 1 {
		t.Fatalf("expected 1 audit, got %d", len(audits))
	}
	if !audits[0].DryRun {
		t.Error("dry_run: got false, want true")
	}
}

func TestRecentDreamAudits_Ordering(t *testing.T) {
	store := newTestStoreForDream(t)

	_ = store.SaveDreamAudit("merge", []int64{1}, 10, "", "first", "r1", false)
	_ = store.SaveDreamAudit("expire", []int64{2}, 0, "", "second", "r2", false)
	_ = store.SaveDreamAudit("promote", []int64{3}, 3, "", "third", "r3", false)

	audits, err := store.RecentDreamAudits(2)
	if err != nil {
		t.Fatalf("RecentDreamAudits: %v", err)
	}
	if len(audits) != 2 {
		t.Fatalf("expected 2 audits (limited), got %d", len(audits))
	}
	// Newest first.
	if audits[0].Operation != "promote" {
		t.Errorf("first audit should be newest: got %q, want %q", audits[0].Operation, "promote")
	}
	if audits[1].Operation != "expire" {
		t.Errorf("second audit: got %q, want %q", audits[1].Operation, "expire")
	}
}

func TestRecentDreamAudits_Empty(t *testing.T) {
	store := newTestStoreForDream(t)

	audits, err := store.RecentDreamAudits(10)
	if err != nil {
		t.Fatalf("RecentDreamAudits: %v", err)
	}
	if len(audits) != 0 {
		t.Errorf("expected 0 audits, got %d", len(audits))
	}
}

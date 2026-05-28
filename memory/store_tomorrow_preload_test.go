package memory

import (
	"path/filepath"
	"testing"
	"time"
)

func newPreloadTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "preload_test.db")
	store, err := NewStore(dbPath, 0)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestTomorrowPreloadLifecycle(t *testing.T) {
	store := newPreloadTestStore(t)

	// No active preload initially.
	p, err := store.ActiveTomorrowPreload()
	if err != nil {
		t.Fatalf("ActiveTomorrowPreload: %v", err)
	}
	if p != nil {
		t.Fatal("expected nil preload before any saves")
	}

	// Save a preload with 48h expiry.
	id, err := store.SaveTomorrowPreload("- Check in about the interview\n- Mood has been low", 48*time.Hour)
	if err != nil {
		t.Fatalf("SaveTomorrowPreload: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero ID")
	}

	// Should now be active.
	p, err = store.ActiveTomorrowPreload()
	if err != nil {
		t.Fatalf("ActiveTomorrowPreload after save: %v", err)
	}
	if p == nil {
		t.Fatal("expected active preload after save")
	}
	if p.ID != id {
		t.Errorf("got ID %d, want %d", p.ID, id)
	}
	if p.Content == "" {
		t.Error("expected non-empty content")
	}

	// Consume it.
	if err := store.ConsumeTomorrowPreload(id); err != nil {
		t.Fatalf("ConsumeTomorrowPreload: %v", err)
	}

	// Should no longer be active.
	p, err = store.ActiveTomorrowPreload()
	if err != nil {
		t.Fatalf("ActiveTomorrowPreload after consume: %v", err)
	}
	if p != nil {
		t.Fatal("expected nil preload after consumption")
	}
}

func TestTomorrowPreloadExpiry(t *testing.T) {
	store := newPreloadTestStore(t)

	// Save with negative expiry — already expired.
	_, err := store.SaveTomorrowPreload("this should be expired", -1*time.Hour)
	if err != nil {
		t.Fatalf("SaveTomorrowPreload: %v", err)
	}

	// Should not be active (expired).
	p, err := store.ActiveTomorrowPreload()
	if err != nil {
		t.Fatalf("ActiveTomorrowPreload: %v", err)
	}
	if p != nil {
		t.Fatal("expected nil preload for expired entry")
	}
}

func TestTomorrowPreloadLatestWins(t *testing.T) {
	store := newPreloadTestStore(t)

	// Save two preloads — the most recent should be returned.
	_, err := store.SaveTomorrowPreload("first preload", 48*time.Hour)
	if err != nil {
		t.Fatalf("SaveTomorrowPreload first: %v", err)
	}
	id2, err := store.SaveTomorrowPreload("second preload", 48*time.Hour)
	if err != nil {
		t.Fatalf("SaveTomorrowPreload second: %v", err)
	}

	p, err := store.ActiveTomorrowPreload()
	if err != nil {
		t.Fatalf("ActiveTomorrowPreload: %v", err)
	}
	if p == nil {
		t.Fatal("expected active preload")
	}
	if p.ID != id2 {
		t.Errorf("got ID %d, want latest %d", p.ID, id2)
	}
}

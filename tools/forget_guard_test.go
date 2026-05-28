package tools

import (
	"path/filepath"
	"testing"
	"time"

	"her/config"
	"her/memory"
)

func newGuardTestStore(t *testing.T) *memory.SQLiteStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "guard_test.db")
	store, err := memory.NewStore(dbPath, 0)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestCanForget(t *testing.T) {
	store := newGuardTestStore(t)
	cfg := config.ForgettingConfig{
		Enabled:              true,
		RequireLowImportance: 3,
		MinAgeDays:           60,
		MinUnusedDays:        60,
	}

	// Use the seed "identity" card (already created by migrations, protected=true).
	protectedCard, err := store.GetCard("identity")
	if err != nil || protectedCard == nil {
		t.Fatalf("GetCard identity: %v", err)
	}
	protectedID, err := store.SaveMemory("user name is Autumn", "identity", "user", 0, 10, nil, nil, "name", "", protectedCard.ID)
	if err != nil {
		t.Fatalf("SaveMemory protected: %v", err)
	}

	// Save a memory in an unprotected card, old and low importance.
	organicCard, err := store.CreateCard("temp-notes", "Temp Notes", "user", 0)
	if err != nil {
		t.Fatalf("CreateCard organic: %v", err)
	}
	eligibleID, err := store.SaveMemory("had coffee on march 1", "event", "user", 0, 2, nil, nil, "coffee", "", organicCard.ID)
	if err != nil {
		t.Fatalf("SaveMemory eligible: %v", err)
	}
	// Backdate to 90 days ago so it passes the age check.
	store.DB().Exec("UPDATE memories SET timestamp = datetime('now', '-90 days') WHERE id = ?", eligibleID)

	// Save a high-importance memory in an unprotected card.
	highImpID, err := store.SaveMemory("user's primary job", "work", "user", 0, 8, nil, nil, "work", "", organicCard.ID)
	if err != nil {
		t.Fatalf("SaveMemory high-imp: %v", err)
	}
	store.DB().Exec("UPDATE memories SET timestamp = datetime('now', '-90 days') WHERE id = ?", highImpID)

	// Save a recent memory (low importance, unprotected, but too new).
	recentID, err := store.SaveMemory("mentioned weather today", "event", "user", 0, 1, nil, nil, "weather", "", organicCard.ID)
	if err != nil {
		t.Fatalf("SaveMemory recent: %v", err)
	}

	tests := []struct {
		name     string
		memID    int64
		wantOK   bool
		wantSub  string
	}{
		{
			name:    "protected card — refused",
			memID:   protectedID,
			wantOK:  false,
			wantSub: "protected",
		},
		{
			name:   "eligible — old, low importance, unprotected",
			memID:  eligibleID,
			wantOK: true,
		},
		{
			name:    "high importance — refused",
			memID:   highImpID,
			wantOK:  false,
			wantSub: "importance",
		},
		{
			name:    "too recent — refused",
			memID:   recentID,
			wantOK:  false,
			wantSub: "days ago",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mem, err := store.GetMemory(tt.memID)
			if err != nil || mem == nil {
				t.Fatalf("GetMemory(%d): %v", tt.memID, err)
			}
			ok, reason := CanForget(mem, store, cfg)
			if ok != tt.wantOK {
				t.Errorf("CanForget = %v (reason: %s), want %v", ok, reason, tt.wantOK)
			}
			if !tt.wantOK && tt.wantSub != "" {
				if reason == "" || !contains(reason, tt.wantSub) {
					t.Errorf("reason %q should contain %q", reason, tt.wantSub)
				}
			}
		})
	}
}

func TestCanForget_RecalledRecently(t *testing.T) {
	store := newGuardTestStore(t)
	cfg := config.ForgettingConfig{
		Enabled:              true,
		RequireLowImportance: 3,
		MinAgeDays:           60,
		MinUnusedDays:        60,
	}

	card, _ := store.CreateCard("temp", "Temp", "user", 0)
	id, _ := store.SaveMemory("old stale note", "event", "user", 0, 2, nil, nil, "note", "", card.ID)
	// Backdate the memory to 90 days ago.
	store.DB().Exec("UPDATE memories SET timestamp = datetime('now', '-90 days') WHERE id = ?", id)
	// But mark it as recalled recently.
	store.MarkMemoriesRecalled([]int64{id})

	mem, _ := store.GetMemory(id)
	ok, reason := CanForget(mem, store, cfg)
	if ok {
		t.Error("expected refusal for recently-recalled memory")
	}
	if !contains(reason, "recalled") {
		t.Errorf("reason %q should mention recall", reason)
	}
}

func TestCanForget_SupersessionChain(t *testing.T) {
	store := newGuardTestStore(t)
	cfg := config.ForgettingConfig{
		Enabled:              true,
		RequireLowImportance: 3,
		MinAgeDays:           60,
		MinUnusedDays:        60,
	}

	card, _ := store.CreateCard("temp", "Temp", "user", 0)
	oldID, _ := store.SaveMemory("works at Panera", "work", "user", 0, 2, nil, nil, "work", "", card.ID)
	newID, _ := store.SaveMemory("works at Cava", "work", "user", 0, 2, nil, nil, "work", "", card.ID)
	store.SupersedeMemory(oldID, newID, "job changed")

	// Backdate both.
	store.DB().Exec("UPDATE memories SET timestamp = datetime('now', '-90 days') WHERE id IN (?, ?)", oldID, newID)

	// The new memory is the head of the chain — should be refused.
	mem, _ := store.GetMemory(newID)
	ok, reason := CanForget(mem, store, cfg)
	if ok {
		t.Error("expected refusal for head of supersession chain")
	}
	if !contains(reason, "supersession") {
		t.Errorf("reason %q should mention supersession", reason)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsSub(s, sub))
}

func containsSub(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Silence the "unused" warning for time import.
var _ = time.Now

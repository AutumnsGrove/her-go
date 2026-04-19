package mood

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"her/memory"
)

func newProposalTestStore(t *testing.T) *memory.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "proposal.db")
	store, err := memory.NewStore(dbPath, 0)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func seedPendingProposal(t *testing.T, store *memory.Store, chatID, msgID int64) int64 {
	t.Helper()
	entry := memory.MoodEntry{
		Kind:    memory.MoodKindMomentary,
		Valence: 3,
		Labels:  []string{"Disappointed"},
		Note:    "letdown",
	}
	blob, _ := json.Marshal(entry)
	id, err := store.SavePendingMoodProposal(&memory.PendingMoodProposal{
		TelegramChatID:    chatID,
		TelegramMessageID: msgID,
		ProposalJSON:      blob,
		ExpiresAt:         time.Now().Add(30 * time.Minute),
	})
	if err != nil {
		t.Fatalf("seed proposal: %v", err)
	}
	return id
}

func TestConfirmProposal_HappyPath(t *testing.T) {
	store := newProposalTestStore(t)
	seedPendingProposal(t, store, 42, 777)

	var editedText string
	edit := func(_ int64, _ int, text string) error {
		editedText = text
		return nil
	}

	entry, err := ConfirmProposal(store, 42, 777, edit)
	if err != nil {
		t.Fatalf("ConfirmProposal: %v", err)
	}
	if entry.Source != memory.MoodSourceConfirmed {
		t.Errorf("Source = %q, want confirmed", entry.Source)
	}
	if entry.Valence != 3 {
		t.Errorf("Valence = %d, want 3 (decoded from proposal)", entry.Valence)
	}

	// The saved row is queryable.
	got, _ := store.LatestMoodEntry("")
	if got == nil || got.ID != entry.ID {
		t.Errorf("latest entry mismatch: %v", got)
	}

	// Proposal status flipped.
	p, _ := store.PendingMoodProposalByMessageID(42, 777)
	if p == nil || p.Status != memory.MoodProposalConfirmed {
		t.Errorf("proposal status = %v, want confirmed", p)
	}

	// Edit callback fired with a friendly confirmation.
	if editedText == "" {
		t.Error("edit callback not invoked")
	}
}

func TestConfirmProposal_UnknownMessageErrors(t *testing.T) {
	store := newProposalTestStore(t)
	_, err := ConfirmProposal(store, 1, 99999, nil)
	if err == nil {
		t.Error("ConfirmProposal(unknown) returned nil error")
	}
}

func TestConfirmProposal_AlreadyConfirmedErrors(t *testing.T) {
	store := newProposalTestStore(t)
	seedPendingProposal(t, store, 42, 777)

	// First confirm succeeds.
	if _, err := ConfirmProposal(store, 42, 777, nil); err != nil {
		t.Fatalf("first confirm: %v", err)
	}
	// Second confirm should fail — proposal already resolved.
	if _, err := ConfirmProposal(store, 42, 777, nil); err == nil {
		t.Error("second ConfirmProposal returned nil error; want status-mismatch")
	}
}

func TestRejectProposal_FlipsStatus(t *testing.T) {
	store := newProposalTestStore(t)
	seedPendingProposal(t, store, 42, 777)

	var editedText string
	edit := func(_ int64, _ int, text string) error {
		editedText = text
		return nil
	}
	if err := RejectProposal(store, 42, 777, edit); err != nil {
		t.Fatalf("RejectProposal: %v", err)
	}
	p, _ := store.PendingMoodProposalByMessageID(42, 777)
	if p.Status != memory.MoodProposalRejected {
		t.Errorf("status = %q, want rejected", p.Status)
	}
	// No mood entry written.
	entries, _ := store.RecentMoodEntries("", 5)
	if len(entries) != 0 {
		t.Errorf("entries = %d, want 0 on reject", len(entries))
	}
	if editedText == "" {
		t.Error("edit callback not invoked")
	}
}

func TestRejectProposal_UnknownMessageErrors(t *testing.T) {
	store := newProposalTestStore(t)
	if err := RejectProposal(store, 1, 99999, nil); err == nil {
		t.Error("RejectProposal(unknown) returned nil error")
	}
}

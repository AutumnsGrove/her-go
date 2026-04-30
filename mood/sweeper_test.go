package mood

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"her/memory"
)

// newSweeperStore opens an embedDim=0 store — the sweeper doesn't
// touch vec_moods.
func newSweeperStore(t *testing.T) memory.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "sweeper.db")
	store, err := memory.NewStore(dbPath, 0)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// editCapture records every Edit call the sweeper makes so tests can
// assert on the message the user would see.
type editCapture struct {
	mu    sync.Mutex
	calls []editCall
}

type editCall struct {
	ChatID    int64
	MessageID int
	Text      string
}

func (e *editCapture) fn() func(chatID int64, messageID int, text string) error {
	return func(chatID int64, messageID int, text string) error {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.calls = append(e.calls, editCall{ChatID: chatID, MessageID: messageID, Text: text})
		return nil
	}
}

func TestProposalSweeper_ExpiresDueProposals(t *testing.T) {
	store := newSweeperStore(t)

	// Seed two proposals: one expired an hour ago, one not yet due.
	now := time.Now().UTC()
	expired, err := store.SavePendingMoodProposal(&memory.PendingMoodProposal{
		TelegramChatID:    42,
		TelegramMessageID: 1001,
		ProposalJSON:      json.RawMessage(`{"valence":3}`),
		ExpiresAt:         now.Add(-1 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := store.SavePendingMoodProposal(&memory.PendingMoodProposal{
		TelegramChatID:    42,
		TelegramMessageID: 1002,
		ProposalJSON:      json.RawMessage(`{"valence":5}`),
		ExpiresAt:         now.Add(1 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	cap := &editCapture{}
	s := &ProposalSweeper{
		Store: store,
		Clock: func() time.Time { return now },
		Edit:  cap.fn(),
	}

	s.Sweep(context.Background())

	// Only the expired proposal should have been edited.
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.calls) != 1 {
		t.Fatalf("edit calls = %d, want 1", len(cap.calls))
	}
	if cap.calls[0].MessageID != 1001 {
		t.Errorf("edited message = %d, want 1001 (expired)", cap.calls[0].MessageID)
	}

	// Status should have flipped to expired in the DB.
	expiredRow, _ := store.PendingMoodProposalByMessageID(42, 1001)
	if expiredRow.Status != memory.MoodProposalExpired {
		t.Errorf("expired proposal status = %q, want %q", expiredRow.Status, memory.MoodProposalExpired)
	}
	// Fresh proposal must still be pending.
	freshRow, _ := store.PendingMoodProposalByMessageID(42, 1002)
	if freshRow.Status != memory.MoodProposalPending {
		t.Errorf("fresh proposal status = %q, want %q", freshRow.Status, memory.MoodProposalPending)
	}

	_, _ = expired, fresh // silence unused (IDs used via PendingMoodProposalByMessageID)
}

// TestProposalSweeper_SurvivesEditFailure — if Telegram returns an
// error (user deleted the message, chat banned, etc.) the DB status
// must still flip so we don't re-try on every sweep forever.
func TestProposalSweeper_SurvivesEditFailure(t *testing.T) {
	store := newSweeperStore(t)

	now := time.Now().UTC()
	_, err := store.SavePendingMoodProposal(&memory.PendingMoodProposal{
		TelegramChatID:    1,
		TelegramMessageID: 77,
		ProposalJSON:      json.RawMessage(`{}`),
		ExpiresAt:         now.Add(-1 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}

	errEdit := func(_ int64, _ int, _ string) error {
		return context.DeadlineExceeded // some network-y error
	}

	s := &ProposalSweeper{
		Store: store,
		Clock: func() time.Time { return now },
		Edit:  errEdit,
	}
	s.Sweep(context.Background())

	got, _ := store.PendingMoodProposalByMessageID(1, 77)
	if got.Status != memory.MoodProposalExpired {
		t.Errorf("status = %q after edit failure, want %q", got.Status, memory.MoodProposalExpired)
	}
}

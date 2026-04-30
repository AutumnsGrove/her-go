package memory

import (
	"path/filepath"
	"testing"
)

// newInboxTestStore opens a fresh temp SQLite with all tables created.
// embedDim=0 means the vec_memories / vec_moods virtual tables are skipped —
// these tests only touch the inbox table.
func newInboxTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "inbox_test.db")
	store, err := NewStore(dbPath, 0)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestSendInbox(t *testing.T) {
	store := newInboxTestStore(t)

	id, err := store.SendInbox("main", "memory", "cleanup", `{"note":"test"}`)
	if err != nil {
		t.Fatalf("SendInbox: %v", err)
	}
	if id == 0 {
		t.Fatal("SendInbox returned id=0, want a real row ID")
	}
}

// TestConsumeInbox_Basic sends two messages to "memory", consumes them,
// and verifies both come back in insertion order (oldest first).
func TestConsumeInbox_Basic(t *testing.T) {
	store := newInboxTestStore(t)

	if _, err := store.SendInbox("main", "memory", "cleanup", `{"a":1}`); err != nil {
		t.Fatalf("SendInbox 1: %v", err)
	}
	if _, err := store.SendInbox("main", "memory", "split", `{"b":2}`); err != nil {
		t.Fatalf("SendInbox 2: %v", err)
	}

	msgs, err := store.ConsumeInbox("memory")
	if err != nil {
		t.Fatalf("ConsumeInbox: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}

	// ConsumeInbox orders by created_at ASC — first in, first out.
	if msgs[0].MsgType != "cleanup" {
		t.Errorf("msgs[0].MsgType = %q, want %q", msgs[0].MsgType, "cleanup")
	}
	if msgs[1].MsgType != "split" {
		t.Errorf("msgs[1].MsgType = %q, want %q", msgs[1].MsgType, "split")
	}

	// Verify the struct fields are populated correctly.
	if msgs[0].Recipient != "memory" {
		t.Errorf("msgs[0].Recipient = %q, want %q", msgs[0].Recipient, "memory")
	}
	if msgs[0].Sender != "main" {
		t.Errorf("msgs[0].Sender = %q, want %q", msgs[0].Sender, "main")
	}
	if msgs[0].Payload != `{"a":1}` {
		t.Errorf("msgs[0].Payload = %q, want %q", msgs[0].Payload, `{"a":1}`)
	}
}

// TestConsumeInbox_OnlyPending verifies that consuming for one recipient
// does not return messages addressed to a different recipient.
func TestConsumeInbox_OnlyPending(t *testing.T) {
	store := newInboxTestStore(t)

	if _, err := store.SendInbox("main", "memory", "cleanup", `{}`); err != nil {
		t.Fatalf("SendInbox to memory: %v", err)
	}
	if _, err := store.SendInbox("main", "main", "result", `{}`); err != nil {
		t.Fatalf("SendInbox to main: %v", err)
	}

	msgs, err := store.ConsumeInbox("memory")
	if err != nil {
		t.Fatalf("ConsumeInbox(memory): %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages for 'memory', want 1", len(msgs))
	}
	if msgs[0].Recipient != "memory" {
		t.Errorf("consumed a message for the wrong recipient: %q", msgs[0].Recipient)
	}

	// The "main" message should still be pending.
	count, err := store.PendingInboxCount("main")
	if err != nil {
		t.Fatalf("PendingInboxCount(main): %v", err)
	}
	if count != 1 {
		t.Errorf("PendingInboxCount(main) = %d, want 1 (message should still be pending)", count)
	}
}

// TestConsumeInbox_ConsumeOnce verifies atomicity: the same messages cannot
// be consumed twice. The first call returns the messages; the second returns
// an empty slice because the rows are already marked 'consumed'.
func TestConsumeInbox_ConsumeOnce(t *testing.T) {
	store := newInboxTestStore(t)

	if _, err := store.SendInbox("main", "memory", "cleanup", `{}`); err != nil {
		t.Fatalf("SendInbox: %v", err)
	}

	first, err := store.ConsumeInbox("memory")
	if err != nil {
		t.Fatalf("first ConsumeInbox: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("first consume: got %d messages, want 1", len(first))
	}

	second, err := store.ConsumeInbox("memory")
	if err != nil {
		t.Fatalf("second ConsumeInbox: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("second consume: got %d messages, want 0 (already consumed)", len(second))
	}
}

// TestConsumeInbox_Empty verifies that consuming from an empty inbox
// returns nil (not an error, not an empty-but-non-nil slice).
// In Go, nil and an empty slice behave the same in range loops, but
// callers doing `if msgs == nil` checks need the nil case to work correctly.
func TestConsumeInbox_Empty(t *testing.T) {
	store := newInboxTestStore(t)

	msgs, err := store.ConsumeInbox("memory")
	if err != nil {
		t.Fatalf("ConsumeInbox on empty inbox: %v", err)
	}
	if msgs != nil {
		t.Errorf("got %v, want nil for empty inbox", msgs)
	}
}

// TestPendingInboxCount sends 3 messages, checks count=3, consumes, checks count=0.
// This also tests that PendingInboxCount only counts 'pending' rows, not consumed ones.
func TestPendingInboxCount(t *testing.T) {
	store := newInboxTestStore(t)

	for i := 0; i < 3; i++ {
		if _, err := store.SendInbox("main", "memory", "cleanup", `{}`); err != nil {
			t.Fatalf("SendInbox %d: %v", i, err)
		}
	}

	count, err := store.PendingInboxCount("memory")
	if err != nil {
		t.Fatalf("PendingInboxCount before consume: %v", err)
	}
	if count != 3 {
		t.Errorf("count before consume = %d, want 3", count)
	}

	if _, err := store.ConsumeInbox("memory"); err != nil {
		t.Fatalf("ConsumeInbox: %v", err)
	}

	count, err = store.PendingInboxCount("memory")
	if err != nil {
		t.Fatalf("PendingInboxCount after consume: %v", err)
	}
	if count != 0 {
		t.Errorf("count after consume = %d, want 0", count)
	}
}

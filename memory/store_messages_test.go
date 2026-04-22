package memory

import (
	"path/filepath"
	"testing"
)

func newMessageTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "msg_test.db")
	store, err := NewStore(dbPath, 0)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestSaveMessage_RoundTrip(t *testing.T) {
	store := newMessageTestStore(t)

	id, err := store.SaveMessage("user", "raw hello", "scrubbed hello", "conv-1")
	if err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}
	if id == 0 {
		t.Fatal("SaveMessage returned id=0, want a real row ID")
	}

	msgs, err := store.RecentMessages("conv-1", 10)
	if err != nil {
		t.Fatalf("RecentMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	m := msgs[0]
	if m.Role != "user" {
		t.Errorf("Role = %q, want %q", m.Role, "user")
	}
	if m.ContentRaw != "raw hello" {
		t.Errorf("ContentRaw = %q, want %q", m.ContentRaw, "raw hello")
	}
	if m.ContentScrubbed != "scrubbed hello" {
		t.Errorf("ContentScrubbed = %q, want %q", m.ContentScrubbed, "scrubbed hello")
	}
	if m.ConversationID != "conv-1" {
		t.Errorf("ConversationID = %q, want %q", m.ConversationID, "conv-1")
	}
}

func TestRecentMessages_OldestFirst(t *testing.T) {
	store := newMessageTestStore(t)

	store.SaveMessage("user", "first", "", "conv-1")
	store.SaveMessage("assistant", "second", "", "conv-1")
	store.SaveMessage("user", "third", "", "conv-1")

	msgs, err := store.RecentMessages("conv-1", 10)
	if err != nil {
		t.Fatalf("RecentMessages: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3", len(msgs))
	}
	if msgs[0].ContentRaw != "first" || msgs[2].ContentRaw != "third" {
		t.Errorf("messages not in chronological order: got %q, %q, %q",
			msgs[0].ContentRaw, msgs[1].ContentRaw, msgs[2].ContentRaw)
	}
}

func TestRecentMessages_Limit(t *testing.T) {
	store := newMessageTestStore(t)

	for i := 0; i < 10; i++ {
		store.SaveMessage("user", "msg", "", "conv-1")
	}

	msgs, err := store.RecentMessages("conv-1", 3)
	if err != nil {
		t.Fatalf("RecentMessages: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3", len(msgs))
	}
}

func TestRecentMessages_ConversationIsolation(t *testing.T) {
	store := newMessageTestStore(t)

	store.SaveMessage("user", "conv1 msg", "", "conv-1")
	store.SaveMessage("user", "conv2 msg", "", "conv-2")

	msgs, err := store.RecentMessages("conv-1", 10)
	if err != nil {
		t.Fatalf("RecentMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages for conv-1, want 1", len(msgs))
	}
	if msgs[0].ContentRaw != "conv1 msg" {
		t.Errorf("got wrong message: %q", msgs[0].ContentRaw)
	}
}

func TestGlobalRecentMessages_CrossConversation(t *testing.T) {
	store := newMessageTestStore(t)

	store.SaveMessage("user", "conv1", "", "conv-1")
	store.SaveMessage("user", "conv2", "", "conv-2")
	store.SaveMessage("user", "conv1 again", "", "conv-1")

	msgs, err := store.GlobalRecentMessages(10)
	if err != nil {
		t.Fatalf("GlobalRecentMessages: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3", len(msgs))
	}
	// Should be oldest-first
	if msgs[0].ContentRaw != "conv1" {
		t.Errorf("first message = %q, want %q", msgs[0].ContentRaw, "conv1")
	}
}

func TestMessagesAfter(t *testing.T) {
	store := newMessageTestStore(t)

	id1, _ := store.SaveMessage("user", "first", "", "conv-1")
	store.SaveMessage("user", "second", "", "conv-1")
	store.SaveMessage("user", "third", "", "conv-1")

	msgs, err := store.MessagesAfter("conv-1", id1)
	if err != nil {
		t.Fatalf("MessagesAfter: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	if msgs[0].ContentRaw != "second" {
		t.Errorf("first after = %q, want %q", msgs[0].ContentRaw, "second")
	}
}

func TestMessagesInRange(t *testing.T) {
	store := newMessageTestStore(t)

	id1, _ := store.SaveMessage("user", "one", "", "conv-1")
	id2, _ := store.SaveMessage("user", "two", "", "conv-1")
	id3, _ := store.SaveMessage("user", "three", "", "conv-1")
	store.SaveMessage("user", "four", "", "conv-1") // outside range

	msgs, err := store.MessagesInRange("conv-1", id1, id3)
	if err != nil {
		t.Fatalf("MessagesInRange: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3", len(msgs))
	}
	_ = id2 // used implicitly — it's in the range
}

func TestUpdateMessageScrubbed(t *testing.T) {
	store := newMessageTestStore(t)

	id, _ := store.SaveMessage("user", "my SSN is 123-45-6789", "", "conv-1")

	err := store.UpdateMessageScrubbed(id, "my SSN is [SSN_REDACTED]")
	if err != nil {
		t.Fatalf("UpdateMessageScrubbed: %v", err)
	}

	msgs, err := store.RecentMessages("conv-1", 1)
	if err != nil {
		t.Fatalf("RecentMessages: %v", err)
	}
	if msgs[0].ContentScrubbed != "my SSN is [SSN_REDACTED]" {
		t.Errorf("scrubbed = %q, want redacted version", msgs[0].ContentScrubbed)
	}
	// Raw should be untouched
	if msgs[0].ContentRaw != "my SSN is 123-45-6789" {
		t.Errorf("raw was modified: %q", msgs[0].ContentRaw)
	}
}

func TestMessageCountSince(t *testing.T) {
	store := newMessageTestStore(t)

	id1, _ := store.SaveMessage("user", "one", "", "conv-1")
	store.SaveMessage("user", "two", "", "conv-1")
	store.SaveMessage("assistant", "reply", "", "conv-1") // assistant, shouldn't count
	store.SaveMessage("user", "three", "", "conv-1")

	count, err := store.MessageCountSince("conv-1", id1)
	if err != nil {
		t.Fatalf("MessageCountSince: %v", err)
	}
	// Only user messages after id1: "two" and "three" (not "reply")
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}
}

func TestLatestConversationID(t *testing.T) {
	store := newMessageTestStore(t)

	store.SaveMessage("user", "old", "", "tg_123_aaa")
	store.SaveMessage("user", "new", "", "tg_123_bbb")

	got := store.LatestConversationID("tg_123")
	if got != "tg_123_bbb" {
		t.Errorf("LatestConversationID = %q, want %q", got, "tg_123_bbb")
	}
}

func TestLatestConversationID_NoMessages(t *testing.T) {
	store := newMessageTestStore(t)

	got := store.LatestConversationID("tg_999")
	if got != "" {
		t.Errorf("LatestConversationID = %q, want empty string", got)
	}
}

func TestUpdateMessageTokenCount(t *testing.T) {
	store := newMessageTestStore(t)

	id, _ := store.SaveMessage("user", "hello", "", "conv-1")
	if err := store.UpdateMessageTokenCount(id, 42); err != nil {
		t.Fatalf("UpdateMessageTokenCount: %v", err)
	}

	msgs, err := store.RecentMessages("conv-1", 1)
	if err != nil {
		t.Fatalf("RecentMessages: %v", err)
	}
	if msgs[0].TokenCount != 42 {
		t.Errorf("TokenCount = %d, want 42", msgs[0].TokenCount)
	}
}

package send_task

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"her/memory"
	"her/tools"
)

// newSendTaskTestStore opens a fresh temp SQLite with all tables created.
// embedDim=0 skips the vector tables — these tests only touch the inbox.
func newSendTaskTestStore(t *testing.T) memory.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "send_task_test.db")
	store, err := memory.NewStore(dbPath, 0)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestHandle_Basic verifies that a simple task lands in the "memory"
// recipient's inbox with the correct msg_type.
func TestHandle_Basic(t *testing.T) {
	store := newSendTaskTestStore(t)
	ctx := &tools.Context{Store: store}

	result := Handle(`{"task_type": "cleanup", "note": "remove stale memories"}`, ctx)

	if strings.Contains(result, "error") {
		t.Fatalf("Handle returned error: %q", result)
	}
	// The success message names both the task type and the inbox ID.
	if !strings.Contains(result, "cleanup") {
		t.Errorf("result %q does not mention task type 'cleanup'", result)
	}
	if !strings.Contains(result, "memory agent") {
		t.Errorf("result %q does not mention 'memory agent'", result)
	}

	// Verify the message actually landed in the inbox.
	msgs, err := store.ConsumeInbox("memory")
	if err != nil {
		t.Fatalf("ConsumeInbox: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d inbox messages, want 1", len(msgs))
	}

	m := msgs[0]
	if m.Sender != "main" {
		t.Errorf("Sender = %q, want %q", m.Sender, "main")
	}
	if m.Recipient != "memory" {
		t.Errorf("Recipient = %q, want %q", m.Recipient, "memory")
	}
	if m.MsgType != "cleanup" {
		t.Errorf("MsgType = %q, want %q", m.MsgType, "cleanup")
	}
}

// TestHandle_WithMemoryIDs verifies that when memory_ids are included in the
// args, the full payload (including the IDs) is stored in the inbox message
// so the memory agent can act on specific memories.
func TestHandle_WithMemoryIDs(t *testing.T) {
	store := newSendTaskTestStore(t)
	ctx := &tools.Context{Store: store}

	result := Handle(`{"task_type": "split", "note": "split this memory", "memory_ids": [7, 42, 99]}`, ctx)

	if strings.Contains(result, "error") {
		t.Fatalf("Handle returned error: %q", result)
	}

	msgs, err := store.ConsumeInbox("memory")
	if err != nil {
		t.Fatalf("ConsumeInbox: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d inbox messages, want 1", len(msgs))
	}

	// The payload is the re-encoded args JSON. Decode it and check the IDs.
	var payload struct {
		TaskType  string  `json:"task_type"`
		Note      string  `json:"note"`
		MemoryIDs []int64 `json:"memory_ids"`
	}
	if err := json.Unmarshal([]byte(msgs[0].Payload), &payload); err != nil {
		t.Fatalf("unmarshaling payload %q: %v", msgs[0].Payload, err)
	}

	if payload.TaskType != "split" {
		t.Errorf("payload.task_type = %q, want %q", payload.TaskType, "split")
	}
	if len(payload.MemoryIDs) != 3 {
		t.Fatalf("payload.memory_ids len = %d, want 3", len(payload.MemoryIDs))
	}
	if payload.MemoryIDs[0] != 7 || payload.MemoryIDs[1] != 42 || payload.MemoryIDs[2] != 99 {
		t.Errorf("payload.memory_ids = %v, want [7 42 99]", payload.MemoryIDs)
	}
}

// TestHandle_InvalidJSON verifies that malformed args return a clear error
// string rather than panicking. This matches the resilience contract in
// dispatch.go: bad tool calls are survivable.
func TestHandle_InvalidJSON(t *testing.T) {
	store := newSendTaskTestStore(t)
	ctx := &tools.Context{Store: store}

	result := Handle(`{"task_type": "cleanup"`, ctx) // truncated JSON

	if !strings.Contains(result, "error") {
		t.Errorf("expected error for malformed JSON, got: %q", result)
	}

	// Nothing should have landed in the inbox.
	count, err := store.PendingInboxCount("memory")
	if err != nil {
		t.Fatalf("PendingInboxCount: %v", err)
	}
	if count != 0 {
		t.Errorf("inbox count = %d, want 0 (bad args should not write to inbox)", count)
	}
}

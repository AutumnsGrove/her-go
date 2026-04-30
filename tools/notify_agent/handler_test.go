package notify_agent

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"her/memory"
	"her/tools"
)

// newTestStore opens a fresh temp SQLite database with all tables created.
// embedDim=0 skips the vec_memories virtual table — inbox tests don't need it.
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

// TestHandle_HappyPath is the core correctness test: both fields set, inbox
// written, DoneCalled true, return string contains the summary.
func TestHandle_HappyPath(t *testing.T) {
	store := newTestStore(t)
	ctx := &tools.Context{Store: store}

	result := Handle(`{"summary": "finished cleanup", "direct_message": "All done!"}`, ctx)

	if strings.Contains(result, "error") {
		t.Fatalf("Handle returned unexpected error: %q", result)
	}
	if !strings.Contains(result, "finished cleanup") {
		t.Errorf("result = %q, want it to contain summary %q", result, "finished cleanup")
	}
	if !ctx.DoneCalled {
		t.Error("DoneCalled = false, want true")
	}
}

// TestHandle_InboxContents verifies the full round-trip: the message that
// lands in the "main" inbox has the right sender, msg_type, and a payload
// JSON that encodes both summary and direct_message.
func TestHandle_InboxContents(t *testing.T) {
	store := newTestStore(t)
	ctx := &tools.Context{Store: store}

	Handle(`{"summary": "memory split done", "direct_message": "Split complete."}`, ctx)

	msgs, err := store.ConsumeInbox("main")
	if err != nil {
		t.Fatalf("ConsumeInbox: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d inbox messages, want 1", len(msgs))
	}

	m := msgs[0]

	if m.Sender != "memory" {
		t.Errorf("Sender = %q, want %q", m.Sender, "memory")
	}
	if m.Recipient != "main" {
		t.Errorf("Recipient = %q, want %q", m.Recipient, "main")
	}
	if m.MsgType != "result" {
		t.Errorf("MsgType = %q, want %q", m.MsgType, "result")
	}

	// The payload must be valid JSON containing the original fields.
	var payload struct {
		Summary       string `json:"summary"`
		DirectMessage string `json:"direct_message"`
	}
	if err := json.Unmarshal([]byte(m.Payload), &payload); err != nil {
		t.Fatalf("unmarshaling payload %q: %v", m.Payload, err)
	}
	if payload.Summary != "memory split done" {
		t.Errorf("payload.summary = %q, want %q", payload.Summary, "memory split done")
	}
	if payload.DirectMessage != "Split complete." {
		t.Errorf("payload.direct_message = %q, want %q", payload.DirectMessage, "Split complete.")
	}
}

// TestHandle_AgentEventCB_Called verifies that when AgentEventCB is wired,
// it receives exactly the summary and direct_message values from the args.
func TestHandle_AgentEventCB_Called(t *testing.T) {
	store := newTestStore(t)

	var gotSummary, gotDirect string
	ctx := &tools.Context{
		Store: store,
		AgentEventCB: func(summary, direct string) {
			gotSummary = summary
			gotDirect = direct
		},
	}

	Handle(`{"summary": "wake up", "direct_message": "Here is your update."}`, ctx)

	if gotSummary != "wake up" {
		t.Errorf("AgentEventCB got summary = %q, want %q", gotSummary, "wake up")
	}
	if gotDirect != "Here is your update." {
		t.Errorf("AgentEventCB got direct = %q, want %q", gotDirect, "Here is your update.")
	}
}

// TestHandle_AgentEventCB_Nil ensures that a nil callback doesn't panic —
// the inbox write and DoneCalled still happen normally.
func TestHandle_AgentEventCB_Nil(t *testing.T) {
	store := newTestStore(t)
	ctx := &tools.Context{Store: store} // AgentEventCB is nil by default

	// Must not panic.
	result := Handle(`{"summary": "no cb", "direct_message": ""}`, ctx)

	if strings.Contains(result, "error") {
		t.Fatalf("Handle returned unexpected error: %q", result)
	}
	if !ctx.DoneCalled {
		t.Error("DoneCalled = false, want true even when AgentEventCB is nil")
	}

	// Inbox should still have the message.
	msgs, err := store.ConsumeInbox("main")
	if err != nil {
		t.Fatalf("ConsumeInbox: %v", err)
	}
	if len(msgs) != 1 {
		t.Errorf("got %d inbox messages, want 1 (inbox write should not depend on callback)", len(msgs))
	}
}

// TestHandle_InvalidJSON verifies that malformed args return a clear error
// string and don't panic. DoneCalled behaviour on parse failure is not
// guaranteed — we only check the error surface.
func TestHandle_InvalidJSON(t *testing.T) {
	store := newTestStore(t)
	ctx := &tools.Context{Store: store}

	result := Handle(`{"summary": "oops"`, ctx) // truncated JSON

	if !strings.Contains(result, "error") {
		t.Errorf("expected error string for malformed JSON, got: %q", result)
	}
}

// TestHandle_DoneCalledAlwaysSet checks the contract stated in the handler
// comment: "notify implies done". Even when the inbox write encounters an
// error (simulated by using a closed store), DoneCalled should be true
// because the event still fires and the agent's turn should end.
//
// Note: we can't easily force an inbox error with a valid store, so this
// test uses a deliberately closed DB to induce a write failure. We
// recover from the expected panic-free error path and verify DoneCalled.
func TestHandle_DoneCalledAfterInboxFailure(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "closed.db")
	store, err := memory.NewStore(dbPath, 0)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	// Close the store before calling Handle so SendInbox will fail.
	_ = store.Close()

	ctx := &tools.Context{Store: store}

	// Must not panic — inbox write failure is logged and swallowed by handler.
	// The handler's contract says DoneCalled is set regardless.
	result := Handle(`{"summary": "done anyway", "direct_message": ""}`, ctx)

	// The handler does not return "error" for inbox failures — it only logs.
	// What matters is DoneCalled is true and we get the success return string.
	_ = result // return value is best-effort; focus on state

	if !ctx.DoneCalled {
		t.Error("DoneCalled = false after inbox write failure, want true")
	}
}

// TestHandle_EmptySummary verifies that an empty summary is a valid edge case
// — the tool should still write to the inbox and set DoneCalled.
func TestHandle_EmptySummary(t *testing.T) {
	store := newTestStore(t)
	ctx := &tools.Context{Store: store}

	result := Handle(`{"summary": "", "direct_message": ""}`, ctx)

	if strings.Contains(result, "error parsing") {
		t.Fatalf("empty summary treated as parse error: %q", result)
	}
	if !ctx.DoneCalled {
		t.Error("DoneCalled = false for empty summary, want true")
	}

	msgs, err := store.ConsumeInbox("main")
	if err != nil {
		t.Fatalf("ConsumeInbox: %v", err)
	}
	if len(msgs) != 1 {
		t.Errorf("got %d inbox messages, want 1", len(msgs))
	}
}

// TestHandle_TableDriven covers return-string and DoneCalled across several
// normal input variants without needing inbox inspection for each one.
func TestHandle_TableDriven(t *testing.T) {
	cases := []struct {
		name           string
		argsJSON       string
		wantDone       bool
		wantContains   string
		wantErrContains string
	}{
		{
			name:         "summary only",
			argsJSON:     `{"summary": "task complete", "direct_message": ""}`,
			wantDone:     true,
			wantContains: "task complete",
		},
		{
			name:         "both fields set",
			argsJSON:     `{"summary": "all good", "direct_message": "Everything went well."}`,
			wantDone:     true,
			wantContains: "all good",
		},
		{
			name:            "totally invalid JSON",
			argsJSON:        `not json at all`,
			wantDone:        false,
			wantErrContains: "error",
		},
		{
			name:            "empty object — no fields",
			argsJSON:        `{}`,
			wantDone:        true,  // empty args are valid; summary just happens to be ""
			wantContains:   "notified driver agent",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			ctx := &tools.Context{Store: store}

			result := Handle(tc.argsJSON, ctx)

			if tc.wantErrContains != "" {
				if !strings.Contains(result, tc.wantErrContains) {
					t.Errorf("result = %q, want it to contain %q", result, tc.wantErrContains)
				}
				return
			}

			if tc.wantContains != "" && !strings.Contains(result, tc.wantContains) {
				t.Errorf("result = %q, want it to contain %q", result, tc.wantContains)
			}
			if ctx.DoneCalled != tc.wantDone {
				t.Errorf("DoneCalled = %v, want %v", ctx.DoneCalled, tc.wantDone)
			}
		})
	}
}

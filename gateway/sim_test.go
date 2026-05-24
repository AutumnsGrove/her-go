package gateway

// Tests for the sim adapter. The sim adapter is the integration-test
// transport — it feeds pre-loaded messages through the pipeline and
// collects results. These tests cover creation, identity methods,
// helpers, and error cases. They do NOT start a full pipeline (that
// would require a real database), so Start() is not called.

import (
	"strings"
	"testing"

	"her/config"
)

// newTestSimAdapter creates a simAdapter with the given messages.
func newTestSimAdapter(t *testing.T, messages []SimMessage) *simAdapter {
	t.Helper()
	cfg := config.AdapterConfig{
		Name: "test-sim",
		Type: "sim",
	}
	a, err := newSimAdapter(cfg, messages)
	if err != nil {
		t.Fatalf("newSimAdapter: %v", err)
	}
	return a.(*simAdapter)
}

// ---- Creation and identity --------------------------------------------------

func TestSimAdapter_NewSimAdapter(t *testing.T) {
	msgs := []SimMessage{
		{Text: "hello"},
		{Text: "how are you?"},
	}
	a := newTestSimAdapter(t, msgs)

	if a == nil {
		t.Fatal("expected non-nil simAdapter")
	}
	// messages should be stored as-is.
	if len(a.messages) != 2 {
		t.Errorf("messages: got %d, want 2", len(a.messages))
	}
	// Done channel must be initialised so callers can block on it.
	if a.Done == nil {
		t.Error("expected Done channel to be initialised")
	}
	// msgCh must be ready to receive.
	if a.msgCh == nil {
		t.Error("expected msgCh to be initialised")
	}
}

func TestSimAdapter_NameReturnsConfigName(t *testing.T) {
	a := newTestSimAdapter(t, nil)
	if a.Name() != "test-sim" {
		t.Errorf("Name(): got %q, want %q", a.Name(), "test-sim")
	}
}

func TestSimAdapter_CapabilitiesEmpty(t *testing.T) {
	a := newTestSimAdapter(t, nil)
	caps := a.Capabilities()
	// The sim adapter intentionally supports nothing — it's a headless
	// integration harness, not a real UI.
	if caps != (CapSet{}) {
		t.Errorf("Capabilities(): got %+v, want empty CapSet", caps)
	}
}

// ---- Results ----------------------------------------------------------------

func TestSimAdapter_ResultsEmptyBeforeRun(t *testing.T) {
	a := newTestSimAdapter(t, []SimMessage{{Text: "hi"}})
	results := a.Results()
	if len(results) != 0 {
		t.Errorf("Results() before run: got %d items, want 0", len(results))
	}
}

func TestSimAdapter_ResultsReturnsCopy(t *testing.T) {
	a := newTestSimAdapter(t, nil)
	// Manually append a result to the internal slice to simulate a completed run.
	a.mu.Lock()
	a.results = append(a.results, SimResult{Input: "test", Reply: "reply"})
	a.mu.Unlock()

	r1 := a.Results()
	r2 := a.Results()

	// Results() must return a copy — mutating it must not affect the adapter.
	if len(r1) != 1 {
		t.Fatalf("expected 1 result, got %d", len(r1))
	}
	r1[0].Reply = "mutated"
	if r2[0].Reply != "reply" {
		t.Error("Results() returned a slice sharing the underlying array — expected a copy")
	}
}

// ---- Adapter interface no-ops -----------------------------------------------

func TestSimAdapter_StopReturnsNil(t *testing.T) {
	a := newTestSimAdapter(t, nil)
	if err := a.Stop(); err != nil {
		t.Errorf("Stop(): unexpected error: %v", err)
	}
}

func TestSimAdapter_SendStatusReturnsNil(t *testing.T) {
	a := newTestSimAdapter(t, nil)
	if err := a.SendStatus("doing stuff..."); err != nil {
		t.Errorf("SendStatus(): unexpected error: %v", err)
	}
}

func TestSimAdapter_StartTypingReturnsNoopFunc(t *testing.T) {
	a := newTestSimAdapter(t, nil)
	cancel := a.StartTyping()
	if cancel == nil {
		t.Error("StartTyping(): expected non-nil cancel func")
	}
	// Should not panic.
	cancel()
}

func TestSimAdapter_RegisterCommandsStored(t *testing.T) {
	a := newTestSimAdapter(t, nil)
	defs := []CommandDef{
		{Name: "help", Description: "Show help"},
		{Name: "stats", Description: "Show stats"},
	}
	a.RegisterCommands(defs)

	if len(a.commands) != 2 {
		t.Errorf("commands: got %d, want 2", len(a.commands))
	}
}

func TestSimAdapter_ReceiveReturnsMsgCh(t *testing.T) {
	a := newTestSimAdapter(t, nil)
	ch := a.Receive()
	if ch == nil {
		t.Error("Receive(): expected non-nil channel")
	}
	// Verify it's actually the same channel — not a new allocation each call.
	if ch != a.msgCh {
		t.Error("Receive(): channel identity mismatch, expected a.msgCh")
	}
}

// ---- loadImage --------------------------------------------------------------

func TestLoadImage_NonExistentFileErrors(t *testing.T) {
	_, _, err := loadImage("/tmp/does-not-exist-her-go-test.png")
	if err == nil {
		t.Error("expected error for non-existent image file, got nil")
	}
}

func TestLoadImage_EmptyPathErrors(t *testing.T) {
	_, _, err := loadImage("")
	if err == nil {
		t.Error("expected error for empty image path, got nil")
	}
}

// ---- truncateSimText --------------------------------------------------------

func TestTruncateSimText_ShortStringUnchanged(t *testing.T) {
	s := "hello world"
	got := truncateSimText(s, 80)
	if got != s {
		t.Errorf("truncateSimText: got %q, want %q", got, s)
	}
}

func TestTruncateSimText_LongStringTruncated(t *testing.T) {
	s := strings.Repeat("x", 100)
	got := truncateSimText(s, 50)
	if len(got) != 53 { // 50 chars + "..."
		t.Errorf("truncateSimText: len=%d, want 53", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("truncateSimText: expected trailing '...', got %q", got)
	}
}

func TestTruncateSimText_NewlinesCollapsed(t *testing.T) {
	s := "line one\nline two\nline three"
	got := truncateSimText(s, 200)
	if strings.Contains(got, "\n") {
		t.Errorf("truncateSimText: expected newlines to be replaced, got %q", got)
	}
}

func TestTruncateSimText_ExactLengthNotTruncated(t *testing.T) {
	s := strings.Repeat("a", 80)
	got := truncateSimText(s, 80)
	if got != s {
		t.Errorf("truncateSimText at exact limit: expected unchanged, got %q", got)
	}
}

// validImageMIME is tested in gradio_test.go — no need to repeat here.

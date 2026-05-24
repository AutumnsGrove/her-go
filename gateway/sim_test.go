package gateway

// Tests for the sim adapter. The sim adapter is the integration-test
// transport — it feeds pre-loaded messages through the pipeline and
// collects results. These tests cover creation, identity methods,
// helpers, and error cases. They do NOT start a full pipeline (that
// would require a real database), so Start() is not called.

import (
	"context"
	"strings"
	"testing"
	"time"

	"her/config"
	"her/tui"
)

// newTestSimAdapter creates a simAdapter with the given messages.
func newTestSimAdapter(t *testing.T, messages []SimMessage) *simAdapter {
	t.Helper()
	cfg := config.AdapterConfig{
		Name: "test-sim",
		Type: "sim",
	}
	a, err := newSimAdapter(cfg, messages, SimTriggers{}, SimOptions{}, nil)
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

// ---- Bus capture tests ------------------------------------------------------

func TestSimAdapter_BusCaptureEnrichesResults(t *testing.T) {
	bus := tui.NewBus()
	defer bus.Close()

	cfg := config.AdapterConfig{Name: "test-sim-bus", Type: "sim"}
	a, err := newSimAdapter(cfg, nil, SimTriggers{}, SimOptions{}, bus)
	if err != nil {
		t.Fatalf("newSimAdapter: %v", err)
	}
	sa := a.(*simAdapter)

	// Start the capture goroutine manually (normally Start() does this).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sa.captureBusEvents(ctx)
	time.Sleep(20 * time.Millisecond) // let goroutine subscribe

	// Simulate a turn's worth of events on the bus.
	bus.Emit(tui.TurnStartEvent{Time: time.Now(), TurnID: 1, UserMessage: "hello"})
	bus.Emit(tui.ToolCallEvent{Time: time.Now(), TurnID: 1, Source: "main", ToolName: "think", Args: `{"thought":"testing"}`, Result: "ok"})
	bus.Emit(tui.ToolCallEvent{Time: time.Now(), TurnID: 1, Source: "memory", ToolName: "save_memory", Result: "saved: user likes Go"})
	bus.Emit(tui.MoodEvent{Time: time.Now(), TurnID: 1, Action: "auto_logged", Valence: 5, Labels: []string{"curious"}, Confidence: 0.8})
	bus.Emit(tui.TurnEndEvent{Time: time.Now(), TurnID: 1, TotalCost: 0.0042, ToolCalls: 2})

	// Wait for the finalized turn to arrive.
	select {
	case tc := <-sa.finishedTurn:
		if tc.cost != 0.0042 {
			t.Errorf("cost: got %f, want 0.0042", tc.cost)
		}
		if tc.toolCalls != 2 {
			t.Errorf("toolCalls: got %d, want 2", tc.toolCalls)
		}
		if tc.moodVerdict != "auto_logged" {
			t.Errorf("moodVerdict: got %q, want %q", tc.moodVerdict, "auto_logged")
		}
		if tc.moodValence != 5 {
			t.Errorf("moodValence: got %d, want 5", tc.moodValence)
		}
		if len(tc.memoriesSaved) != 1 {
			t.Errorf("memoriesSaved: got %d, want 1", len(tc.memoriesSaved))
		}
		if len(tc.toolLog) != 2 {
			t.Errorf("toolLog: got %d entries, want 2", len(tc.toolLog))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for finalized turn capture")
	}
}

func TestSimAdapter_ExtractThought(t *testing.T) {
	args := `{"thought":"User seems curious about Go"}`
	got := extractThought(args)
	if got != "User seems curious about Go" {
		t.Errorf("extractThought: got %q, want %q", got, "User seems curious about Go")
	}
}

func TestSimAdapter_ToolIcon(t *testing.T) {
	if toolIcon("think") != "🧠" {
		t.Error("expected brain emoji for think")
	}
	if toolIcon("save_memory") != "💾" {
		t.Error("expected floppy for save_memory")
	}
	if toolIcon("unknown_tool") != "🔧" {
		t.Error("expected wrench for unknown tool")
	}
}

// validImageMIME is tested in gradio_test.go — no need to repeat here.

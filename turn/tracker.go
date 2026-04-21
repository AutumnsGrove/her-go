package turn

import (
	"sync"
	"time"

	"her/logger"
	"her/tui"
)

var log = logger.WithPrefix("turn")

// Metrics accumulates turn-wide numbers across all phases. Each
// PhaseHandle.Done() merges its phase's PhaseMetrics into this.
type Metrics struct {
	TotalCost     float64
	ToolCalls     int
	MemoriesSaved int
}

// Tracker manages the lifecycle of one user-message turn. Created by
// bot/run_agent.go at the start of each turn. Phases call Begin() to
// join the turn and Done() on the returned handle when they finish.
// When all phases complete, Tracker emits TurnEndEvent with
// accumulated metrics.
//
// Think of it like Python's asyncio.gather() — "wait for N things
// to finish, then do cleanup." The difference is we also accumulate
// data (cost, memories saved) alongside the wait.
type Tracker struct {
	turnID      int64
	startTime   time.Time
	bus         *tui.Bus // nil-safe (sim mode)
	userMessage string
	convID      string

	mu      sync.Mutex
	pending int     // ref count of active phases
	metrics Metrics // accumulated across all phases
	closed  bool    // true after final phase completes

	stopTypingFn func()    // the raw closer from bot
	typingOnce   sync.Once // prevents double-close panic

	done chan struct{} // closed when pending hits 0
}

// NewTracker creates a Tracker for a turn and emits TurnStartEvent.
//
//   - turnID: the triggering message's DB ID (used for event routing)
//   - bus: the TUI event bus (nil in sim mode — events silently drop)
//   - stopTypingFn: function that stops the typing indicator goroutine
//     (nil in sim mode — typing management becomes a no-op)
//   - userMessage: truncated user text for TUI display
//   - convID: conversation ID for the TurnStartEvent
func NewTracker(turnID int64, bus *tui.Bus, stopTypingFn func(), userMessage, convID string) *Tracker {
	t := &Tracker{
		turnID:       turnID,
		startTime:    time.Now(),
		bus:          bus,
		userMessage:  userMessage,
		convID:       convID,
		stopTypingFn: stopTypingFn,
		done:         make(chan struct{}),
	}

	// Emit the turn start event. The Tracker owns this now — callers
	// no longer emit TurnStartEvent manually.
	t.emit(tui.TurnStartEvent{
		Time:           t.startTime,
		TurnID:         turnID,
		UserMessage:    userMessage,
		ConversationID: convID,
	})

	return t
}

// Begin registers an active phase by name and returns a PhaseHandle
// the agent uses for event emission and signaling completion.
// Increments the pending ref count.
//
// Must be called BEFORE launching the goroutine that runs the phase.
// This prevents a race where all known phases are done but a new one
// is about to start — if you Begin() inside the goroutine, there's
// a window where pending=0 and TurnEndEvent fires prematurely.
//
// If the phase name isn't in the registry, Begin still works (like
// trace.Board's fallback for unknown slots). A warning is logged.
func (t *Tracker) Begin(phaseName string) *PhaseHandle {
	phase, known := LookupPhase(phaseName)
	if !known {
		log.Warn("turn.Begin: unregistered phase", "name", phaseName)
		phase = Phase{Name: phaseName}
	}

	t.mu.Lock()
	t.pending++
	t.mu.Unlock()

	return &PhaseHandle{
		tracker: t,
		name:    phaseName,
		phase:   phase,
	}
}

// StopTyping stops the typing indicator. Safe to call multiple times
// from any goroutine (sync.Once). Called automatically when all
// phases complete, but the main phase should also call it explicitly
// after the reply is sent so typing stops immediately.
func (t *Tracker) StopTyping() {
	t.typingOnce.Do(func() {
		if t.stopTypingFn != nil {
			t.stopTypingFn()
		}
	})
}

// Wait blocks until all phases complete. Used by sim mode for
// deterministic ordering — the sim needs background agents to finish
// before asserting on results. In production this is never called;
// the completion logic fires asynchronously inside phaseDone.
func (t *Tracker) Wait() {
	<-t.done
}

// TurnID returns the turn's triggering message ID.
func (t *Tracker) TurnID() int64 {
	return t.turnID
}

// phaseDone is called by PhaseHandle.Done(). Decrements the pending
// ref count, accumulates metrics, and fires TurnEndEvent + StopTyping
// when pending hits zero.
func (t *Tracker) phaseDone(name string, m PhaseMetrics) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.metrics.TotalCost += m.Cost
	t.metrics.ToolCalls += m.ToolCalls
	t.metrics.MemoriesSaved += m.MemoriesSaved

	t.pending--
	if t.pending < 0 {
		// Should never happen — means Done() was called without Begin().
		log.Error("turn: negative pending count", "phase", name, "pending", t.pending)
		t.pending = 0
	}

	if t.pending == 0 && !t.closed {
		t.closed = true

		// Emit TurnEndEvent with accumulated metrics from all phases.
		t.emit(tui.TurnEndEvent{
			Time:          time.Now(),
			TurnID:        t.turnID,
			ElapsedMs:     time.Since(t.startTime).Milliseconds(),
			TotalCost:     t.metrics.TotalCost,
			ToolCalls:     t.metrics.ToolCalls,
			MemoriesSaved: t.metrics.MemoriesSaved,
		})

		// Stop typing (no-op if already stopped after reply delivery).
		// Launched in a goroutine so sync.Once doesn't block while
		// we're holding t.mu — the Once might contend briefly with
		// a concurrent StopTyping call from the reply tool.
		go t.StopTyping()

		// Unblock Wait() callers (sim mode).
		close(t.done)
	}
}

// emit is a nil-safe helper for emitting events to the bus.
func (t *Tracker) emit(e tui.Event) {
	if t.bus != nil {
		t.bus.Emit(e)
	}
}

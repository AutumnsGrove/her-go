package turn

import (
	"sync"
	"time"

	"her/tui"
)

// PhaseMetrics is what an agent reports when calling Done(). The
// Tracker accumulates these across all phases for the final
// TurnEndEvent.
type PhaseMetrics struct {
	Cost          float64
	ToolCalls     int
	MemoriesSaved int
}

// PhaseHandle is the interface an agent uses to participate in a turn.
// Returned by Tracker.Begin(). The agent calls EmitToolCall() as it
// works, then Done() when finished.
//
// In Python terms, this is like a context manager — you enter (Begin)
// and exit (Done), and the handle gives you methods to use in between.
// The key difference: Go doesn't have `with` blocks, so you manage
// the lifecycle manually (typically via `defer handle.Done(...)`).
type PhaseHandle struct {
	tracker  *Tracker
	name     string
	phase    Phase
	doneOnce sync.Once
}

// Done signals this phase is complete and merges its metrics into
// the Tracker's accumulator. Safe to call multiple times — only the
// first call takes effect (sync.Once). Subsequent calls log a warning.
//
// Typical usage:
//
//	phase := tracker.Begin("memory")
//	go func() {
//	    defer phase.Done(turn.PhaseMetrics{Cost: totalCost})
//	    RunMemoryAgent(...)
//	}()
func (h *PhaseHandle) Done(m PhaseMetrics) {
	h.doneOnce.Do(func() {
		h.tracker.phaseDone(h.name, m)
	})
}

// Emit sends a TUI event through the tracker's bus. Nil-safe — if
// the tracker has no bus (sim mode), the event is silently dropped.
func (h *PhaseHandle) Emit(e tui.Event) {
	h.tracker.emit(e)
}

// EmitToolCall emits a ToolCallEvent tagged with this phase's name
// as the Source. The TUI routes events to the correct content group
// based on Source — "memory" goes to MemoryLines, "mood" to
// MoodLines, everything else to ToolLines.
//
// This auto-sets TurnID and Source so callers don't have to.
func (h *PhaseHandle) EmitToolCall(toolName, args, result string, isError bool) {
	h.tracker.emit(tui.ToolCallEvent{
		Time:     time.Now(),
		TurnID:   h.tracker.turnID,
		Source:   h.name,
		ToolName: toolName,
		Args:     args,
		Result:   result,
		IsError:  isError,
	})
}

// TurnID returns the turn's triggering message ID. Agents need this
// for store.SaveMetric, trace events, and other per-turn bookkeeping.
func (h *PhaseHandle) TurnID() int64 {
	return h.tracker.turnID
}

// StopTyping stops the typing indicator via the tracker. The main
// phase calls this after the reply is sent so typing stops
// immediately rather than waiting for all background phases to
// complete.
func (h *PhaseHandle) StopTyping() {
	h.tracker.StopTyping()
}

// Name returns the phase name (e.g. "main", "memory", "mood").
func (h *PhaseHandle) Name() string {
	return h.name
}

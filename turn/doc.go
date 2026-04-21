// Package turn manages the lifecycle of a single user-message turn,
// from the moment a message arrives through the reply and all
// background work (memory extraction, mood inference, persona
// evolution) that follows.
//
// # Shape
//
//   - Phase — a named lifecycle stage (main, memory, mood) with a
//     render order. Registered from init() in the owning package,
//     same pattern as trace.Stream.
//
//   - Tracker — per-turn coordinator. Created by bot/run_agent.go,
//     manages ref-counting of active phases, typing indicator
//     lifecycle, and the TurnEndEvent emission when all phases
//     complete.
//
//   - PhaseHandle — agent-facing interface returned by
//     Tracker.Begin(). Auto-tags TUI events with the correct
//     TurnID and Source so events route to the right content group
//     in the TUI. Agents call Done() to signal completion.
//
// # Why a registry
//
// Same motivation as trace/: adding a new background agent should
// mean adding ONE init() call in its own package, not editing
// RunParams, run_agent.go, model.go, and view.go. The Tracker
// discovers registered phases at construction time; agents that
// the bot doesn't know about still work as long as they register
// a phase and call Begin()/Done().
//
// # Ref-counting
//
// Each Begin() increments a pending counter; each Done() decrements
// it and merges that phase's metrics (cost, memories, tool calls)
// into the Tracker's accumulator. When pending hits zero:
//  1. StopTyping fires (sync.Once — safe from any goroutine)
//  2. TurnEndEvent emits with accumulated metrics
//  3. The done channel closes (unblocks Wait for sim mode)
//
// This replaces the old arrangement where TurnEndEvent fired from
// bot/run_agent.go before background agents finished, producing
// incomplete cost numbers.
package turn

// Package trace is a lightweight observability inbox for agent
// activity. Every agent in the system — main, memory, mood, and
// anything added later — can register a trace stream at init time
// and push status lines into it. A Board ties those streams to a
// single user-visible surface (a Telegram message, a TUI panel, a
// log writer), re-rendering the full transcript whenever any stream
// changes.
//
// # Shape
//
//   - Stream — a named slot with a render order (lower = earlier)
//     and an optional display label. Registered from init() in the
//     owning package.
//
//   - Board — per-turn inbox. Each Set(slot, content) overwrites a
//     slot and triggers one edit of the backing surface. Thread-safe;
//     multiple agent goroutines can write concurrently.
//
// # Why a registry
//
// Same motivation as scheduler extensions: adding a new agent should
// mean adding ONE init() call in its own package, not editing every
// consumer. The bot's trace wiring is a single function that loops
// over Streams() and makes callbacks on demand — agents the bot
// doesn't know about still render correctly as long as they register
// a stream and push content.
//
// # Ordering
//
// Streams render in ascending Order, ties broken by registration
// order (stable sort). This means "main" always shows before
// "memory" / "mood" regardless of which post-turn agent finishes
// first, so the visual layout is deterministic.
package trace

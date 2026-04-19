package bot

import (
	"strings"
	"sync"
)

// TraceBoard is a thread-safe "inbox" for observability messages that
// share a single Telegram message. Each agent writes into its own
// named slot; the board re-renders the full message whenever any
// slot changes.
//
// The problem this solves: multiple goroutines (main agent, memory
// agent, mood agent — potentially more later) all want to show what
// they're doing in the chat, but editing one shared message
// concurrently would clobber each other. Before the inbox, mem+main
// were serialized by an implicit mutex; adding mood meant extending
// that with no structure. The board generalizes: N slots, one lock,
// one render function, all agents happy.
//
// Slots render in insertion order, separated by a horizontal rule,
// so the first agent to write shows up first. That matches how the
// turn unfolds in time (main agent speaks, then memory and mood fire
// in parallel post-reply).
type TraceBoard struct {
	mu    sync.Mutex
	order []string          // insertion-order slot names
	slots map[string]string // slot name → current content

	// edit is called on every change with the full rendered message.
	// The bot wires this to a Telegram edit; tests can capture into
	// a slice. Must tolerate being called from multiple goroutines —
	// the board calls it under the mutex, so callers don't have to
	// synchronize.
	edit func(text string)
}

// Separator between slots. Matches the separator the old dual-agent
// helper used so the rendered message looks the same.
const traceBoardSeparator = "\n\n─────────────\n"

// NewTraceBoard returns a TraceBoard ready to accept Set calls. Pass
// an edit function that pushes to the target (a Telegram message, a
// test capture, etc.).
func NewTraceBoard(edit func(text string)) *TraceBoard {
	return &TraceBoard{
		slots: map[string]string{},
		edit:  edit,
	}
}

// Set writes (or overwrites) the contents of a named slot and
// triggers a re-render. Calling with an empty content string clears
// the slot — useful when an agent decides it has nothing worth
// showing after all.
//
// Thread-safe. Multiple goroutines may call Set concurrently; each
// edit the board does is serialized.
func (b *TraceBoard) Set(slot, content string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	_, existed := b.slots[slot]
	if content == "" {
		if !existed {
			return
		}
		delete(b.slots, slot)
		b.order = removeString(b.order, slot)
	} else {
		if !existed {
			b.order = append(b.order, slot)
		}
		b.slots[slot] = content
	}
	b.render()
}

// Snapshot returns the current full message that would be rendered.
// Useful for assertions and for callers that need the text for
// something besides the edit (e.g. logging).
func (b *TraceBoard) Snapshot() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.combineLocked()
}

// render assembles the message from every slot in insertion order
// and invokes the edit callback. Must be called with b.mu held.
func (b *TraceBoard) render() {
	if b.edit == nil {
		return
	}
	b.edit(b.combineLocked())
}

// combineLocked joins every slot's content with the separator. Caller
// must hold b.mu.
func (b *TraceBoard) combineLocked() string {
	if len(b.order) == 0 {
		return ""
	}
	parts := make([]string, 0, len(b.order))
	for _, slot := range b.order {
		parts = append(parts, b.slots[slot])
	}
	return strings.Join(parts, traceBoardSeparator)
}

// removeString is a tiny slice helper — returns a new slice with the
// first matching string removed. Used when a slot is cleared so its
// position doesn't linger in the order list.
func removeString(in []string, s string) []string {
	for i, x := range in {
		if x == s {
			return append(in[:i], in[i+1:]...)
		}
	}
	return in
}

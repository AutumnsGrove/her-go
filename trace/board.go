package trace

import (
	"strings"
	"sync"
)

// Separator rendered between slots. Chosen to match the visual style
// of older ad-hoc trace messages so the UX transition is invisible.
const slotSeparator = "\n\n─────────────\n"

// Board is the per-turn trace inbox. Each agent writes into its own
// slot via Set; the Board re-renders the full message whenever any
// slot changes and pushes the edit to the backing surface (usually a
// Telegram message). Thread-safe — multiple goroutines may Set
// concurrently; edits are serialized under the Board's mutex.
type Board struct {
	mu    sync.Mutex
	slots map[string]string // slot Name → current content

	// unknownOrder tracks slots whose Name isn't in the registry.
	// Lets us still render them (fallback ordering) instead of
	// losing their content.
	unknownOrder []string

	// edit is called on every content change with the full rendered
	// message. Kept as a function so tests, file loggers, and
	// Telegram edits all plug in the same way.
	edit func(text string)
}

// NewBoard returns a Board ready to accept Set calls. Pass an edit
// function that pushes to the target (a Telegram message, a test
// capture, etc.). A nil edit function is accepted — Board is still
// usable for its Snapshot method.
func NewBoard(edit func(text string)) *Board {
	return &Board{
		slots: map[string]string{},
		edit:  edit,
	}
}

// Set writes (or overwrites) the contents of a named slot and
// triggers a re-render. Empty content clears the slot — useful when
// an agent decides it has nothing worth showing.
func (b *Board) Set(name, content string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	_, existed := b.slots[name]
	if content == "" {
		if !existed {
			return
		}
		delete(b.slots, name)
		b.unknownOrder = removeString(b.unknownOrder, name)
	} else {
		if !existed {
			// Track the slot for fallback ordering if it isn't
			// in the registry. Registered slots use Stream.Order;
			// unknown slots trail behind in insertion order.
			if _, known := LookupStream(name); !known {
				b.unknownOrder = append(b.unknownOrder, name)
			}
		}
		b.slots[name] = content
	}
	b.rerenderLocked()
}

// Snapshot returns the current full message that would be rendered.
// Useful for assertions and for callers that need the text for
// something besides the edit (e.g. logging).
func (b *Board) Snapshot() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.combineLocked()
}

// rerenderLocked assembles the message and invokes edit. Caller
// must hold b.mu. Split from combineLocked so Snapshot can reuse
// the formatter without re-firing the edit callback.
func (b *Board) rerenderLocked() {
	if b.edit == nil {
		return
	}
	b.edit(b.combineLocked())
}

// combineLocked joins every non-empty slot with the separator,
// respecting registry order. Streams registered via trace.Register
// render in ascending Order; any slot NOT in the registry renders
// afterward, in the order it was first Set. Caller must hold b.mu.
func (b *Board) combineLocked() string {
	if len(b.slots) == 0 {
		return ""
	}

	parts := make([]string, 0, len(b.slots))

	// Registered streams first, in Order.
	for _, s := range Streams() {
		content, ok := b.slots[s.Name]
		if !ok {
			continue
		}
		parts = append(parts, formatSlot(s, content))
	}

	// Unknown slots last, preserving insertion order.
	for _, name := range b.unknownOrder {
		content, ok := b.slots[name]
		if !ok {
			continue
		}
		parts = append(parts, content)
	}

	return strings.Join(parts, slotSeparator)
}

// formatSlot renders one slot: Label on its own line (if present)
// followed by the slot's content body.
func formatSlot(s Stream, content string) string {
	if s.Label == "" {
		return content
	}
	return s.Label + "\n" + content
}

// removeString is a tiny slice helper — returns a new slice with the
// first matching string removed.
func removeString(in []string, s string) []string {
	for i, x := range in {
		if x == s {
			return append(in[:i], in[i+1:]...)
		}
	}
	return in
}

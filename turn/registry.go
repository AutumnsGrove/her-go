package turn

import (
	"fmt"
	"sort"
	"sync"
)

// Phase declares a named lifecycle stage in a turn. Packages register
// their phases at init() time; the Tracker queries the registry when
// agents call Begin(). One phase per agent — duplicate names panic
// on Register (programmer error, same philosophy as trace.Register).
//
// Think of this like a Python class decorator that registers a plugin
// at import time — except in Go, init() runs automatically when the
// package is imported, so a blank import does the job.
type Phase struct {
	// Name is the key used by Tracker.Begin and PhaseHandle.
	// Must be non-empty and unique across all registered phases.
	Name string

	// Order controls render order in the TUI — lower renders
	// earlier. "main" registers at 100, memory at 200, mood at
	// 300; a new agent slotting between memory and mood would
	// register at, say, 250.
	Order int

	// Emoji is the single-character icon for this phase, used by
	// both the TUI content group header and the Telegram trace
	// board. Defined here so there's one source of truth.
	Emoji string

	// Label is the display name rendered in the TUI content group
	// header (e.g. "memory", "mood"). Empty means no label.
	Label string
}

var (
	registryMu sync.RWMutex
	registry   []Phase
	registered = map[string]struct{}{}
)

// Register adds a Phase to the global registry. Panics on duplicate
// Name or empty Name — both are programmer errors worth catching at
// startup rather than silently losing events at runtime.
//
// Typical call site:
//
//	func init() {
//	    turn.Register(turn.Phase{Name: "memory", Order: 200, Label: "memory"})
//	}
func Register(p Phase) {
	if p.Name == "" {
		panic("turn.Register: empty Name")
	}

	registryMu.Lock()
	defer registryMu.Unlock()

	if _, dup := registered[p.Name]; dup {
		panic(fmt.Sprintf("turn.Register: duplicate phase %q", p.Name))
	}
	registered[p.Name] = struct{}{}
	registry = append(registry, p)
}

// Phases returns the registered phases sorted by Order (ascending).
// Ties preserve registration order (sort.SliceStable). Returns a
// copy so callers can't mutate the registry.
func Phases() []Phase {
	registryMu.RLock()
	defer registryMu.RUnlock()

	out := make([]Phase, len(registry))
	copy(out, registry)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Order < out[j].Order
	})
	return out
}

// LookupPhase returns the phase with the given Name, or false when
// none is registered. Used by the Tracker when Begin() is called —
// an unregistered phase still works (with a logged warning) but
// won't have ordering metadata.
func LookupPhase(name string) (Phase, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	for _, p := range registry {
		if p.Name == name {
			return p, true
		}
	}
	return Phase{}, false
}

// resetRegistryForTest clears the registry. Tests that register
// phases ad-hoc use this to isolate from global init() registrations.
// Lowercase so it's only accessible from the turn package's tests.
func resetRegistryForTest() {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry = nil
	registered = map[string]struct{}{}
}

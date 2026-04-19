package trace

import (
	"fmt"
	"sort"
	"sync"
)

// Stream declares a named slot in the trace board. Packages register
// their streams at init() time; the bot queries the registry when
// wiring per-turn callbacks. One stream per agent — reuse is a
// programmer error (duplicate names panic on Register).
type Stream struct {
	// Name is the slot key used by Board.Set. Must be non-empty and
	// unique across all registered streams.
	Name string

	// Order controls render order — lower renders earlier. "main"
	// registers at 100, memory at 200, mood at 300; a new agent
	// slotting between memory and mood would register at, say, 250.
	Order int

	// Label is the display header rendered above the stream's
	// content (e.g. "🧩 memory"). Empty = no header; the stream's
	// body renders raw. Typically only main leaves this empty
	// because its content is the primary turn transcript.
	Label string
}

var (
	registryMu sync.RWMutex
	registry   []Stream
	registered = map[string]struct{}{}
)

// Register adds a Stream to the global registry. Panics on duplicate
// Name or empty Name — both are programmer errors worth crashing on
// at startup rather than silently losing traces at runtime.
//
// Typical call site:
//
//	func init() {
//	    trace.Register(trace.Stream{Name: "memory", Order: 200, Label: "🧩 memory"})
//	}
func Register(s Stream) {
	if s.Name == "" {
		panic("trace.Register: empty Name")
	}

	registryMu.Lock()
	defer registryMu.Unlock()

	if _, dup := registered[s.Name]; dup {
		panic(fmt.Sprintf("trace.Register: duplicate stream %q", s.Name))
	}
	registered[s.Name] = struct{}{}
	registry = append(registry, s)
}

// Streams returns the registered streams sorted by Order (ascending).
// Ties preserve registration order (sort.SliceStable). Returns a
// copy so callers can't mutate the registry.
func Streams() []Stream {
	registryMu.RLock()
	defer registryMu.RUnlock()

	out := make([]Stream, len(registry))
	copy(out, registry)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Order < out[j].Order
	})
	return out
}

// LookupStream returns the stream with the given Name, or false when
// none is registered. Used by the Board when rendering — an unknown
// slot can still be rendered (fallback ordering) but won't get a
// Label prepended.
func LookupStream(name string) (Stream, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	for _, s := range registry {
		if s.Name == name {
			return s, true
		}
	}
	return Stream{}, false
}

// resetRegistryForTest clears the registry. Tests that register
// streams ad-hoc use this to isolate from global init() registrations.
// Lowercase so it's only accessible from the trace package's tests.
func resetRegistryForTest() {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry = nil
	registered = map[string]struct{}{}
}

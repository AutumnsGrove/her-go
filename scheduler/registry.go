package scheduler

import (
	"fmt"
	"sort"
	"sync"
)

// The registry holds every Handler registered at init() time. It's a
// package-level singleton because extensions self-register from their
// own packages via Register() — same pattern the tools package uses.
//
// Go note: we use sync.RWMutex instead of sync.Mutex because reads
// (lookup during task dispatch) vastly outnumber writes (registration
// happens only at startup). RWMutex lets many goroutines read
// concurrently but serializes writes. Like Python's threading.RLock
// but split into read and write lock halves.
var (
	registryMu sync.RWMutex
	registry   = map[string]Handler{}
)

// Register adds a Handler to the global registry. Every extension calls
// this from its own package init() function, which runs automatically
// before main() starts. Panics if two handlers register the same Kind —
// that's a programmer error worth crashing on, not a runtime condition.
//
// Usage pattern, from e.g. mood/rollup_task.go:
//
//	func init() {
//	    scheduler.Register(&dailyRollupHandler{})
//	}
//
// This is analogous to Python's entry-point / plugin registration, but
// fully static: extensions are compiled into the binary, not loaded at
// runtime. Hot-reload happens at the task.yaml layer instead.
func Register(h Handler) {
	if h == nil {
		panic("scheduler.Register: nil handler")
	}
	kind := h.Kind()
	if kind == "" {
		panic("scheduler.Register: handler returned empty Kind()")
	}

	registryMu.Lock()
	defer registryMu.Unlock()

	if existing, ok := registry[kind]; ok {
		panic(fmt.Sprintf(
			"scheduler.Register: duplicate kind %q (already registered by %T; now %T)",
			kind, existing, h,
		))
	}
	registry[kind] = h
}

// lookup returns the handler for a given kind, or nil if none is
// registered. Callers must treat nil as "unknown kind" (usually a sign
// that a handler was deleted or renamed but DB rows survived the change).
func lookup(kind string) Handler {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return registry[kind]
}

// registeredKinds returns the sorted list of every registered kind.
// Used by the loader to walk each handler once at startup.
func registeredKinds() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()

	kinds := make([]string, 0, len(registry))
	for k := range registry {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	return kinds
}

// resetRegistryForTest clears the registry. Only used from tests —
// production code never needs it. Exported via a `_test.go` helper
// only; lowercase keeps it package-private.
func resetRegistryForTest() {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry = map[string]Handler{}
}

package agent_engine

import "testing"

// forEachLoopBackend runs fn once per loop implementation.
//
// The engine's whole migration contract is "the same EngineConfig produces the
// same observable result on either loop", so the existing test suite is run
// against both implementations rather than being duplicated. SetLoopBackend
// mutates package state, hence the cleanup — t.Cleanup still runs when the
// subtest fails with t.Fatal, which plain defer would not guarantee.
func forEachLoopBackend(t *testing.T, fn func(t *testing.T)) {
	t.Helper()
	for _, name := range []string{backendLegacy, backendNova} {
		t.Run(name, func(t *testing.T) {
			SetLoopBackend(name, nil)
			t.Cleanup(func() { SetLoopBackend(backendLegacy, nil) })
			fn(t)
		})
	}
}

// currentBackend reports which loop the table is on, for the assertions that
// legitimately differ. Named so a reader sees why a branch exists.
func currentBackend(t *testing.T) string {
	t.Helper()
	return backendFor("test")
}

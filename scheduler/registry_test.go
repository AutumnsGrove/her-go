package scheduler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// fakeHandler is a minimal Handler for registry tests. It doesn't
// actually execute anything useful; we only care that Register and
// lookup route by kind correctly.
type fakeHandler struct {
	kind       string
	configPath string
}

func (f *fakeHandler) Kind() string       { return f.kind }
func (f *fakeHandler) ConfigPath() string { return f.configPath }
func (f *fakeHandler) Execute(_ context.Context, _ json.RawMessage, _ *Deps) error {
	return nil
}

// withCleanRegistry isolates each test — the registry is a package-level
// global, so tests would otherwise interfere with each other. Uses
// t.Cleanup so the reset runs even when the test fails.
func withCleanRegistry(t *testing.T) {
	t.Helper()
	resetRegistryForTest()
	t.Cleanup(resetRegistryForTest)
}

func TestRegister_HappyPath(t *testing.T) {
	withCleanRegistry(t)

	h := &fakeHandler{kind: "test_kind"}
	Register(h)

	got := lookup("test_kind")
	if got == nil {
		t.Fatal("lookup returned nil after Register")
	}
	if got.Kind() != "test_kind" {
		t.Errorf("lookup().Kind() = %q, want %q", got.Kind(), "test_kind")
	}
}

func TestLookup_UnknownKindReturnsNil(t *testing.T) {
	withCleanRegistry(t)

	if got := lookup("nobody_registered_this"); got != nil {
		t.Errorf("lookup(unknown) = %v, want nil", got)
	}
}

func TestRegister_NilPanics(t *testing.T) {
	withCleanRegistry(t)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Register(nil) did not panic")
		}
	}()
	Register(nil)
}

func TestRegister_EmptyKindPanics(t *testing.T) {
	withCleanRegistry(t)

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Register(empty kind) did not panic")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "empty Kind") {
			t.Errorf("panic message = %q, want substring %q", msg, "empty Kind")
		}
	}()
	Register(&fakeHandler{kind: ""})
}

func TestRegister_DuplicateKindPanics(t *testing.T) {
	withCleanRegistry(t)

	Register(&fakeHandler{kind: "taken"})

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("second Register(same kind) did not panic")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "duplicate kind") {
			t.Errorf("panic message = %q, want substring %q", msg, "duplicate kind")
		}
	}()
	Register(&fakeHandler{kind: "taken"})
}

func TestRegisteredKinds_SortedAndComplete(t *testing.T) {
	withCleanRegistry(t)

	// Intentionally register out of order to verify sorting.
	Register(&fakeHandler{kind: "zebra"})
	Register(&fakeHandler{kind: "alpha"})
	Register(&fakeHandler{kind: "mango"})

	got := registeredKinds()
	want := []string{"alpha", "mango", "zebra"}

	if len(got) != len(want) {
		t.Fatalf("registeredKinds() len = %d, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("registeredKinds()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

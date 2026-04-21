package turn

import (
	"strings"
	"testing"
)

// withCleanRegistry isolates tests from each other (and from
// init()-time registrations in other packages). Same pattern as
// trace/registry_test.go.
func withCleanRegistry(t *testing.T) {
	t.Helper()
	resetRegistryForTest()
	t.Cleanup(resetRegistryForTest)
}

func TestRegister_HappyPath(t *testing.T) {
	withCleanRegistry(t)

	Register(Phase{Name: "main", Order: 100, Label: "agent"})
	got, ok := LookupPhase("main")
	if !ok {
		t.Fatal("lookup returned false")
	}
	if got.Order != 100 || got.Label != "agent" {
		t.Errorf("got %+v, want {main 100 agent}", got)
	}
}

func TestRegister_EmptyNamePanics(t *testing.T) {
	withCleanRegistry(t)
	defer func() {
		if r := recover(); r == nil {
			t.Error("Register(empty name) did not panic")
		}
	}()
	Register(Phase{Name: ""})
}

func TestRegister_DuplicatePanics(t *testing.T) {
	withCleanRegistry(t)
	Register(Phase{Name: "main", Order: 100})
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("second Register(same name) did not panic")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "duplicate") {
			t.Errorf("panic msg = %q, want 'duplicate' substring", msg)
		}
	}()
	Register(Phase{Name: "main", Order: 200})
}

func TestPhases_SortedByOrder(t *testing.T) {
	withCleanRegistry(t)
	Register(Phase{Name: "mood", Order: 300})
	Register(Phase{Name: "main", Order: 100})
	Register(Phase{Name: "memory", Order: 200})

	got := Phases()
	want := []string{"main", "memory", "mood"}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	for i, name := range want {
		if got[i].Name != name {
			t.Errorf("[%d] = %q, want %q", i, got[i].Name, name)
		}
	}
}

func TestPhases_StableOnTies(t *testing.T) {
	withCleanRegistry(t)
	Register(Phase{Name: "first", Order: 100})
	Register(Phase{Name: "second", Order: 100})
	Register(Phase{Name: "third", Order: 100})

	got := Phases()
	want := []string{"first", "second", "third"}
	for i, name := range want {
		if got[i].Name != name {
			t.Errorf("[%d] = %q, want %q (stable sort broken?)",
				i, got[i].Name, name)
		}
	}
}

func TestPhases_ReturnsCopy(t *testing.T) {
	withCleanRegistry(t)
	Register(Phase{Name: "main", Order: 100})
	got := Phases()
	got[0].Name = "MUTATED"

	fresh := Phases()
	if fresh[0].Name != "main" {
		t.Errorf("Phases() returned a slice sharing registry backing array: %v", fresh)
	}
}

func TestLookupPhase_Unknown(t *testing.T) {
	withCleanRegistry(t)
	_, ok := LookupPhase("ghost")
	if ok {
		t.Error("LookupPhase returned ok=true for unregistered name")
	}
}

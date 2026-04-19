package trace

import (
	"strings"
	"testing"
)

// withCleanRegistry isolates tests from each other (and from
// init()-time registrations in other packages). Wraps a test body in
// registry reset on entry AND exit.
func withCleanRegistry(t *testing.T) {
	t.Helper()
	resetRegistryForTest()
	t.Cleanup(resetRegistryForTest)
}

func TestRegister_HappyPath(t *testing.T) {
	withCleanRegistry(t)

	Register(Stream{Name: "main", Order: 100, Label: "main"})
	got, ok := LookupStream("main")
	if !ok {
		t.Fatal("lookup returned false")
	}
	if got.Order != 100 || got.Label != "main" {
		t.Errorf("got %+v, want {main 100 main}", got)
	}
}

func TestRegister_EmptyNamePanics(t *testing.T) {
	withCleanRegistry(t)
	defer func() {
		if r := recover(); r == nil {
			t.Error("Register(empty name) did not panic")
		}
	}()
	Register(Stream{Name: ""})
}

func TestRegister_DuplicatePanics(t *testing.T) {
	withCleanRegistry(t)
	Register(Stream{Name: "main", Order: 100})
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
	Register(Stream{Name: "main", Order: 200})
}

func TestStreams_SortedByOrder(t *testing.T) {
	withCleanRegistry(t)
	// Register out of order — registry should stable-sort on output.
	Register(Stream{Name: "mood", Order: 300})
	Register(Stream{Name: "main", Order: 100})
	Register(Stream{Name: "memory", Order: 200})

	got := Streams()
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

func TestStreams_StableOnTies(t *testing.T) {
	withCleanRegistry(t)
	// Same order → registration order should win.
	Register(Stream{Name: "first", Order: 100})
	Register(Stream{Name: "second", Order: 100})
	Register(Stream{Name: "third", Order: 100})

	got := Streams()
	want := []string{"first", "second", "third"}
	for i, name := range want {
		if got[i].Name != name {
			t.Errorf("[%d] = %q, want %q (stable sort broken?)",
				i, got[i].Name, name)
		}
	}
}

func TestStreams_ReturnsCopy(t *testing.T) {
	withCleanRegistry(t)
	Register(Stream{Name: "main", Order: 100})
	got := Streams()
	got[0].Name = "MUTATED"

	fresh := Streams()
	if fresh[0].Name != "main" {
		t.Errorf("Streams() returned a slice sharing registry backing array: %v", fresh)
	}
}

func TestLookupStream_Unknown(t *testing.T) {
	withCleanRegistry(t)
	_, ok := LookupStream("ghost")
	if ok {
		t.Error("LookupStream returned ok=true for unregistered name")
	}
}

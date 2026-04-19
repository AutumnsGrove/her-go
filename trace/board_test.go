package trace

import (
	"strings"
	"sync"
	"testing"
)

func captureEdit() (func(string), *[]string) {
	var mu sync.Mutex
	var captured []string
	return func(text string) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, text)
	}, &captured
}

func TestBoard_SingleSlot_RoundTrip(t *testing.T) {
	withCleanRegistry(t)
	Register(Stream{Name: "main", Order: 100})

	edit, calls := captureEdit()
	b := NewBoard(edit)
	b.Set("main", "hello")

	if got := b.Snapshot(); got != "hello" {
		t.Errorf("Snapshot = %q, want %q", got, "hello")
	}
	if len(*calls) != 1 {
		t.Errorf("edit fired %d times, want 1", len(*calls))
	}
}

// Registry order controls render order regardless of insertion order.
// Without this, mood finishing before memory would render mood first,
// which makes the chat UX unpredictable.
func TestBoard_RespectsRegistryOrderNotInsertionOrder(t *testing.T) {
	withCleanRegistry(t)
	Register(Stream{Name: "main", Order: 100})
	Register(Stream{Name: "memory", Order: 200, Label: "🧩 memory"})
	Register(Stream{Name: "mood", Order: 300, Label: "🎭 mood"})

	b := NewBoard(nil)
	// Insert out of order: mood first, then memory, then main.
	b.Set("mood", "MOOD_BODY")
	b.Set("memory", "MEM_BODY")
	b.Set("main", "MAIN_BODY")

	snap := b.Snapshot()
	mainIdx := strings.Index(snap, "MAIN_BODY")
	memIdx := strings.Index(snap, "MEM_BODY")
	moodIdx := strings.Index(snap, "MOOD_BODY")

	if mainIdx < 0 || memIdx < 0 || moodIdx < 0 {
		t.Fatalf("missing content in snapshot:\n%s", snap)
	}
	if !(mainIdx < memIdx && memIdx < moodIdx) {
		t.Errorf("order wrong: main(%d) mem(%d) mood(%d)\n---\n%s",
			mainIdx, memIdx, moodIdx, snap)
	}
}

func TestBoard_LabelPrefixes(t *testing.T) {
	withCleanRegistry(t)
	Register(Stream{Name: "memory", Order: 200, Label: "🧩 memory"})

	b := NewBoard(nil)
	b.Set("memory", "saved 3 facts")

	snap := b.Snapshot()
	if !strings.HasPrefix(snap, "🧩 memory\n") {
		t.Errorf("snapshot should start with label: %q", snap)
	}
	if !strings.Contains(snap, "saved 3 facts") {
		t.Errorf("body missing: %q", snap)
	}
}

func TestBoard_EmptyContentClearsSlot(t *testing.T) {
	withCleanRegistry(t)
	Register(Stream{Name: "main", Order: 100})
	Register(Stream{Name: "memory", Order: 200, Label: "🧩 memory"})

	b := NewBoard(nil)
	b.Set("main", "hi")
	b.Set("memory", "saved")
	b.Set("main", "")

	snap := b.Snapshot()
	if strings.Contains(snap, "hi") {
		t.Errorf("cleared slot still present: %q", snap)
	}
	if !strings.Contains(snap, "saved") {
		t.Errorf("other slot lost: %q", snap)
	}
}

// Unknown streams (not registered) should still render — we never
// want to silently drop content — but they sort after registered
// ones in insertion order.
func TestBoard_UnknownSlotsRenderAfterRegistered(t *testing.T) {
	withCleanRegistry(t)
	Register(Stream{Name: "main", Order: 100})

	b := NewBoard(nil)
	b.Set("surprise", "UNREG_BODY")
	b.Set("main", "MAIN_BODY")

	snap := b.Snapshot()
	if !strings.Contains(snap, "MAIN_BODY") || !strings.Contains(snap, "UNREG_BODY") {
		t.Fatalf("missing content: %q", snap)
	}
	if strings.Index(snap, "MAIN_BODY") > strings.Index(snap, "UNREG_BODY") {
		t.Errorf("unknown slot rendered before registered one:\n%s", snap)
	}
}

func TestBoard_ConcurrentSetsSafe(t *testing.T) {
	withCleanRegistry(t)
	Register(Stream{Name: "main", Order: 100})
	Register(Stream{Name: "memory", Order: 200, Label: "🧩 memory"})
	Register(Stream{Name: "mood", Order: 300, Label: "🎭 mood"})

	b := NewBoard(nil)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); b.Set("main", "MAIN") }()
		go func() { defer wg.Done(); b.Set("memory", "MEM") }()
		go func() { defer wg.Done(); b.Set("mood", "MOOD") }()
	}
	wg.Wait()

	snap := b.Snapshot()
	for _, want := range []string{"MAIN", "MEM", "MOOD"} {
		if !strings.Contains(snap, want) {
			t.Errorf("missing %q after concurrent writes: %s", want, snap)
		}
	}
}

func TestBoard_NilEditSafe(t *testing.T) {
	withCleanRegistry(t)
	Register(Stream{Name: "main", Order: 100})
	b := NewBoard(nil)
	b.Set("main", "content")
	if got := b.Snapshot(); got != "content" {
		t.Errorf("Snapshot = %q, want %q", got, "content")
	}
}

package bot

import (
	"strings"
	"sync"
	"testing"
)

// captureEdit returns an edit function that stores every rendered
// text in a slice for later inspection.
func captureEdit() (func(string), *[]string) {
	var mu sync.Mutex
	var captured []string
	return func(text string) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, text)
	}, &captured
}

func TestTraceBoard_SingleSlotRoundTrip(t *testing.T) {
	edit, calls := captureEdit()
	b := NewTraceBoard(edit)
	b.Set("main", "hello")

	if got := b.Snapshot(); got != "hello" {
		t.Errorf("Snapshot = %q, want %q", got, "hello")
	}
	if len(*calls) != 1 {
		t.Errorf("edit calls = %d, want 1", len(*calls))
	}
}

func TestTraceBoard_MultipleSlotsPreserveInsertionOrder(t *testing.T) {
	edit, _ := captureEdit()
	b := NewTraceBoard(edit)

	// Insert in this order: main, memory, mood.
	b.Set("main", "MAIN-1")
	b.Set("memory", "MEM-1")
	b.Set("mood", "MOOD-1")

	// Updating an existing slot should NOT move it in the order.
	b.Set("main", "MAIN-2")

	snap := b.Snapshot()
	mainIdx := strings.Index(snap, "MAIN-2")
	memIdx := strings.Index(snap, "MEM-1")
	moodIdx := strings.Index(snap, "MOOD-1")

	if mainIdx < 0 || memIdx < 0 || moodIdx < 0 {
		t.Fatalf("snapshot missing content: %q", snap)
	}
	if !(mainIdx < memIdx && memIdx < moodIdx) {
		t.Errorf("slot order wrong: main(%d) mem(%d) mood(%d)\n---\n%s",
			mainIdx, memIdx, moodIdx, snap)
	}

	// Separator appears between slots — 2 slots → 1 sep, 3 → 2 seps.
	sepCount := strings.Count(snap, strings.TrimSpace(traceBoardSeparator))
	if sepCount != 2 {
		t.Errorf("separator count = %d, want 2", sepCount)
	}
}

func TestTraceBoard_EmptyContentClearsSlot(t *testing.T) {
	edit, _ := captureEdit()
	b := NewTraceBoard(edit)
	b.Set("main", "hi")
	b.Set("memory", "mem")
	b.Set("main", "")

	snap := b.Snapshot()
	if strings.Contains(snap, "hi") {
		t.Errorf("snapshot still contains cleared slot: %q", snap)
	}
	if !strings.Contains(snap, "mem") {
		t.Errorf("snapshot missing remaining slot: %q", snap)
	}
	// Only one slot left → no separator.
	if strings.Contains(snap, strings.TrimSpace(traceBoardSeparator)) {
		t.Errorf("separator present with only 1 slot: %q", snap)
	}
}

func TestTraceBoard_EmptyContentOnMissingSlotIsNoOp(t *testing.T) {
	edit, calls := captureEdit()
	b := NewTraceBoard(edit)
	b.Set("ghost", "")
	if len(*calls) != 0 {
		t.Errorf("edit fired for clear-of-missing-slot; want no-op")
	}
	if got := b.Snapshot(); got != "" {
		t.Errorf("Snapshot = %q, want empty", got)
	}
}

func TestTraceBoard_ConcurrentSetsDontRace(t *testing.T) {
	// Run under `go test -race` — this test catches missing locking.
	edit, calls := captureEdit()
	b := NewTraceBoard(edit)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(3)
		go func(i int) { defer wg.Done(); b.Set("main", "MAIN") }(i)
		go func(i int) { defer wg.Done(); b.Set("memory", "MEM") }(i)
		go func(i int) { defer wg.Done(); b.Set("mood", "MOOD") }(i)
	}
	wg.Wait()

	// Final state must contain all three slots in insertion order.
	snap := b.Snapshot()
	for _, want := range []string{"MAIN", "MEM", "MOOD"} {
		if !strings.Contains(snap, want) {
			t.Errorf("final snapshot missing %q: %q", want, snap)
		}
	}

	if len(*calls) == 0 {
		t.Error("no edit calls captured")
	}
}

func TestTraceBoard_NilEditSafe(t *testing.T) {
	// Board without an edit function should still accept writes and
	// support Snapshot — useful in tests that want the board's state
	// without a target.
	b := NewTraceBoard(nil)
	b.Set("main", "content")
	if got := b.Snapshot(); got != "content" {
		t.Errorf("Snapshot = %q, want %q", got, "content")
	}
}

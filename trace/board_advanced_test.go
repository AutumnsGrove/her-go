package trace

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// Each Set must fire exactly one edit. No spurious renders, no
// missed renders. The test asserts on count so a regression that
// double-fires or drops updates surfaces here.
func TestBoard_EditFiresExactlyOncePerSet(t *testing.T) {
	withCleanRegistry(t)
	Register(Stream{Name: "main", Order: 100})

	var count atomic.Int32
	b := NewBoard(func(string) { count.Add(1) })

	b.Set("main", "a")
	b.Set("main", "b")
	b.Set("main", "c")

	if got := count.Load(); got != 3 {
		t.Errorf("edit fired %d times after 3 Sets, want 3", got)
	}
}

// Snapshot is a read — it must never fire the edit callback, even
// when content is present. Without this guarantee, naive
// observability callers would double-edit Telegram.
func TestBoard_SnapshotDoesNotFireEdit(t *testing.T) {
	withCleanRegistry(t)
	Register(Stream{Name: "main", Order: 100})

	var count atomic.Int32
	b := NewBoard(func(string) { count.Add(1) })
	b.Set("main", "body")

	before := count.Load()
	_ = b.Snapshot()
	_ = b.Snapshot()
	_ = b.Snapshot()
	if got := count.Load(); got != before {
		t.Errorf("Snapshot fired edit: count went from %d to %d", before, got)
	}
}

// Separators appear only BETWEEN slots — not before the first or
// after the last, and not inside a single-slot render.
func TestBoard_SeparatorCountMatchesSlotCount(t *testing.T) {
	withCleanRegistry(t)
	Register(Stream{Name: "s1", Order: 100})
	Register(Stream{Name: "s2", Order: 200})
	Register(Stream{Name: "s3", Order: 300})

	sep := strings.TrimSpace(slotSeparator)

	b := NewBoard(nil)

	// 0 slots set → 0 separators, empty snapshot.
	if got := b.Snapshot(); got != "" {
		t.Errorf("empty board snapshot = %q, want empty", got)
	}

	// 1 slot → 0 separators.
	b.Set("s1", "one")
	if n := strings.Count(b.Snapshot(), sep); n != 0 {
		t.Errorf("1 slot, separator count = %d, want 0", n)
	}

	// 2 slots → 1 separator.
	b.Set("s2", "two")
	if n := strings.Count(b.Snapshot(), sep); n != 1 {
		t.Errorf("2 slots, separator count = %d, want 1", n)
	}

	// 3 slots → 2 separators.
	b.Set("s3", "three")
	if n := strings.Count(b.Snapshot(), sep); n != 2 {
		t.Errorf("3 slots, separator count = %d, want 2", n)
	}
}

// Setting the same slot twice overwrites; it must not append. A
// prior regression would show up as "body1body2" style content.
func TestBoard_RepeatedSetOverwritesNotAppends(t *testing.T) {
	withCleanRegistry(t)
	Register(Stream{Name: "main", Order: 100})

	b := NewBoard(nil)
	b.Set("main", "first")
	b.Set("main", "second")

	snap := b.Snapshot()
	if strings.Contains(snap, "first") {
		t.Errorf("old content lingered after overwrite: %q", snap)
	}
	if !strings.Contains(snap, "second") {
		t.Errorf("new content missing: %q", snap)
	}
}

// Re-setting an unknown slot must not add a duplicate entry to the
// fallback order list. Without this guard, the slot would render
// twice in the final message.
func TestBoard_UnknownSlotIdempotentInsertion(t *testing.T) {
	withCleanRegistry(t)

	b := NewBoard(nil)
	b.Set("ghost", "v1")
	b.Set("ghost", "v2")
	b.Set("ghost", "v3")

	snap := b.Snapshot()
	// Content appears exactly once.
	if strings.Count(snap, "v3") != 1 {
		t.Errorf("ghost slot rendered %d times, want 1: %q",
			strings.Count(snap, "v3"), snap)
	}
	// Old values don't linger.
	if strings.Contains(snap, "v1") || strings.Contains(snap, "v2") {
		t.Errorf("old content leaked: %q", snap)
	}
}

// Clearing a slot that was never set should be a silent no-op, not
// a panic or spurious edit.
func TestBoard_ClearUnknownSlotSilent(t *testing.T) {
	withCleanRegistry(t)
	var count atomic.Int32
	b := NewBoard(func(string) { count.Add(1) })

	b.Set("ghost", "") // never set before — should no-op

	if got := count.Load(); got != 0 {
		t.Errorf("edit fired for clear-of-missing: %d times", got)
	}
	if got := b.Snapshot(); got != "" {
		t.Errorf("Snapshot = %q, want empty", got)
	}
}

// Clearing an unknown slot that WAS set should remove it from the
// render. Same behavior as clearing a registered slot, just through
// the fallback path.
func TestBoard_ClearUnknownSlotRemovesContent(t *testing.T) {
	withCleanRegistry(t)
	b := NewBoard(nil)
	b.Set("unk", "visible")
	b.Set("unk", "")

	if got := b.Snapshot(); got != "" {
		t.Errorf("cleared unknown slot still rendering: %q", got)
	}
}

// Two unknown slots should render in insertion order.
func TestBoard_UnknownSlotsPreserveInsertionOrder(t *testing.T) {
	withCleanRegistry(t)
	b := NewBoard(nil)
	b.Set("first", "F")
	b.Set("second", "S")

	snap := b.Snapshot()
	if strings.Index(snap, "F") > strings.Index(snap, "S") {
		t.Errorf("insertion order lost among unknowns:\n%s", snap)
	}
}

// Snapshot must not deadlock while Set is running concurrently —
// both take b.mu, so a reentrancy bug would lock up the test.
func TestBoard_ConcurrentSnapshotAndSet(t *testing.T) {
	withCleanRegistry(t)
	Register(Stream{Name: "main", Order: 100})

	b := NewBoard(nil)
	done := make(chan struct{})

	// Continuous setters.
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				b.Set("main", "x")
			}
		}()
	}

	// Continuous snapshotters.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = b.Snapshot()
			}
		}()
	}

	go func() {
		wg.Wait()
		close(done)
	}()

	<-done // if this hangs, the lock has a reentrancy or cycle bug
}

// HTML labels (the registry ones use "<b>...</b>") must round-trip
// unchanged. The board doesn't transform label strings; Telegram
// HTML mode handles the rendering.
func TestBoard_LabelWithHTMLRoundTrips(t *testing.T) {
	withCleanRegistry(t)
	Register(Stream{Name: "memory", Order: 200, Label: "🧩 <b>memory</b>"})

	b := NewBoard(nil)
	b.Set("memory", "saved 3 facts")

	snap := b.Snapshot()
	if !strings.HasPrefix(snap, "🧩 <b>memory</b>\n") {
		t.Errorf("HTML label not preserved verbatim: %q", snap)
	}
}

// Empty registry + unknown slots should still render. Useful for
// tests that boot with the registry cleared.
func TestBoard_WorksWithEmptyRegistry(t *testing.T) {
	withCleanRegistry(t)
	// No Register calls.

	b := NewBoard(nil)
	b.Set("a", "A")
	b.Set("b", "B")

	snap := b.Snapshot()
	if !strings.Contains(snap, "A") || !strings.Contains(snap, "B") {
		t.Fatalf("empty-registry render missing content: %q", snap)
	}
	// Insertion order applies since neither is registered.
	if strings.Index(snap, "A") > strings.Index(snap, "B") {
		t.Errorf("insertion order wrong with empty registry:\n%s", snap)
	}
}

// Registration + lookup round-trips in both order and content.
func TestLookupStream_AfterRegister(t *testing.T) {
	withCleanRegistry(t)
	s := Stream{Name: "custom", Order: 500, Label: "🔍 custom"}
	Register(s)

	got, ok := LookupStream("custom")
	if !ok {
		t.Fatal("LookupStream returned false for just-registered stream")
	}
	if got != s {
		t.Errorf("LookupStream = %+v, want %+v", got, s)
	}
}

// Streams() on a fresh registry returns an empty slice (never nil
// spooks a caller iterating it).
func TestStreams_EmptyRegistry(t *testing.T) {
	withCleanRegistry(t)
	got := Streams()
	if got == nil {
		t.Error("Streams() = nil on empty registry; want empty slice")
	}
	if len(got) != 0 {
		t.Errorf("Streams() = %v, want empty", got)
	}
}

// Exercising the full three-slot production layout: main first, then
// memory, then mood — regardless of the order each one ran. Treats
// the trace.Stream API as the contract and asserts a concrete visual
// for a real-world message.
func TestBoard_ProductionThreeSlotLayout(t *testing.T) {
	withCleanRegistry(t)
	// Mirror the real production registrations exactly — same Names,
	// Orders, and Labels as the agent + mood packages register at
	// init(). Keeps this test honest as a documentation reference.
	Register(Stream{Name: "main", Order: 100, Label: "🛠️ <b>main</b>"})
	Register(Stream{Name: "memory", Order: 200, Label: "🧩 <b>memory</b>"})
	Register(Stream{Name: "mood", Order: 300, Label: "🎭 <b>mood</b>"})

	b := NewBoard(nil)
	// Simulate agents finishing out of order — mood runs first (a
	// real-world possibility since mood + memory are parallel post-reply).
	b.Set("mood", "✅ auto-logged #17")
	b.Set("memory", "saved 1 fact")
	b.Set("main", "→ think\n→ reply\n→ done")

	snap := b.Snapshot()
	mainIdx := strings.Index(snap, "🛠️")
	memoryIdx := strings.Index(snap, "🧩")
	moodIdx := strings.Index(snap, "🎭")

	if mainIdx < 0 || memoryIdx < 0 || moodIdx < 0 {
		t.Fatalf("missing labels: main=%d memory=%d mood=%d\n---\n%s",
			mainIdx, memoryIdx, moodIdx, snap)
	}
	if !(mainIdx < memoryIdx && memoryIdx < moodIdx) {
		t.Errorf("layout not main→memory→mood: %d %d %d\n---\n%s",
			mainIdx, memoryIdx, moodIdx, snap)
	}
}

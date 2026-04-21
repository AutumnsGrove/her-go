package turn

import (
	"sync"
	"testing"
	"time"

	"her/tui"
)

// captureBus creates a tui.Bus and returns a function that drains
// events of a specific type. Useful for asserting on TurnStartEvent
// and TurnEndEvent emission.
func captureBus() (*tui.Bus, func() []tui.Event) {
	bus := tui.NewBus()
	ch := bus.Subscribe(64)

	var mu sync.Mutex
	var events []tui.Event

	// Drain in background — the bus channel is unbuffered in some
	// implementations, so we need an active consumer.
	go func() {
		for ev := range ch {
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
		}
	}()

	drain := func() []tui.Event {
		// Give the goroutine a moment to process queued events.
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		out := make([]tui.Event, len(events))
		copy(out, events)
		return out
	}

	return bus, drain
}

// --- Ref-counting ---

func TestTracker_SinglePhase_DoneEmitsTurnEnd(t *testing.T) {
	withCleanRegistry(t)
	Register(Phase{Name: "main", Order: 100})

	bus, drain := captureBus()
	tracker := NewTracker(42, bus, nil, "hello", "conv-1")

	phase := tracker.Begin("main")
	phase.Done(PhaseMetrics{Cost: 0.05, ToolCalls: 3, MemoriesSaved: 1})

	events := drain()

	// Should have TurnStartEvent (from NewTracker) + TurnEndEvent (from Done).
	var starts, ends int
	for _, ev := range events {
		switch ev.(type) {
		case tui.TurnStartEvent:
			starts++
		case tui.TurnEndEvent:
			ends++
		}
	}
	if starts != 1 {
		t.Errorf("TurnStartEvent count = %d, want 1", starts)
	}
	if ends != 1 {
		t.Errorf("TurnEndEvent count = %d, want 1", ends)
	}
}

func TestTracker_MultiPhase_TurnEndAfterLast(t *testing.T) {
	withCleanRegistry(t)
	Register(Phase{Name: "main", Order: 100})
	Register(Phase{Name: "memory", Order: 200})
	Register(Phase{Name: "mood", Order: 300})

	bus, drain := captureBus()
	tracker := NewTracker(42, bus, nil, "hello", "conv-1")

	main := tracker.Begin("main")
	mem := tracker.Begin("memory")
	mood := tracker.Begin("mood")

	// Complete main and memory — TurnEnd should NOT fire yet.
	main.Done(PhaseMetrics{Cost: 0.10})
	mem.Done(PhaseMetrics{Cost: 0.02, MemoriesSaved: 2})

	earlyEvents := drain()
	for _, ev := range earlyEvents {
		if _, ok := ev.(tui.TurnEndEvent); ok {
			t.Fatal("TurnEndEvent fired before all phases complete")
		}
	}

	// Complete mood — TurnEnd should fire now.
	mood.Done(PhaseMetrics{Cost: 0.01})

	allEvents := drain()
	var endEvent *tui.TurnEndEvent
	for _, ev := range allEvents {
		if e, ok := ev.(tui.TurnEndEvent); ok {
			endEvent = &e
		}
	}
	if endEvent == nil {
		t.Fatal("TurnEndEvent never fired after all phases completed")
	}
}

func TestTracker_MetricsAccumulation(t *testing.T) {
	withCleanRegistry(t)
	Register(Phase{Name: "main", Order: 100})
	Register(Phase{Name: "memory", Order: 200})

	bus, drain := captureBus()
	tracker := NewTracker(42, bus, nil, "", "")

	main := tracker.Begin("main")
	mem := tracker.Begin("memory")

	main.Done(PhaseMetrics{Cost: 0.10, ToolCalls: 5, MemoriesSaved: 0})
	mem.Done(PhaseMetrics{Cost: 0.03, ToolCalls: 2, MemoriesSaved: 3})

	events := drain()
	var endEvent *tui.TurnEndEvent
	for _, ev := range events {
		if e, ok := ev.(tui.TurnEndEvent); ok {
			endEvent = &e
		}
	}
	if endEvent == nil {
		t.Fatal("no TurnEndEvent")
	}

	// Metrics should be the sum across both phases.
	if endEvent.TotalCost < 0.12 || endEvent.TotalCost > 0.14 {
		t.Errorf("TotalCost = %f, want ~0.13", endEvent.TotalCost)
	}
	if endEvent.ToolCalls != 7 {
		t.Errorf("ToolCalls = %d, want 7", endEvent.ToolCalls)
	}
	if endEvent.MemoriesSaved != 3 {
		t.Errorf("MemoriesSaved = %d, want 3", endEvent.MemoriesSaved)
	}
}

// --- Double Done idempotency ---

func TestPhaseHandle_DoubleDone_SecondIsNoop(t *testing.T) {
	withCleanRegistry(t)
	Register(Phase{Name: "main", Order: 100})

	bus, drain := captureBus()
	tracker := NewTracker(42, bus, nil, "", "")

	phase := tracker.Begin("main")
	phase.Done(PhaseMetrics{Cost: 0.05})
	phase.Done(PhaseMetrics{Cost: 9.99}) // should be ignored

	events := drain()
	var endCount int
	for _, ev := range events {
		if e, ok := ev.(tui.TurnEndEvent); ok {
			endCount++
			// Should have first call's cost, not second.
			if e.TotalCost > 0.06 {
				t.Errorf("TotalCost = %f, second Done leaked through", e.TotalCost)
			}
		}
	}
	if endCount != 1 {
		t.Errorf("TurnEndEvent count = %d, want 1", endCount)
	}
}

// --- Concurrency ---

func TestTracker_ConcurrentDone(t *testing.T) {
	withCleanRegistry(t)
	Register(Phase{Name: "a", Order: 100})
	Register(Phase{Name: "b", Order: 200})
	Register(Phase{Name: "c", Order: 300})

	bus, drain := captureBus()
	tracker := NewTracker(42, bus, nil, "", "")

	a := tracker.Begin("a")
	b := tracker.Begin("b")
	c := tracker.Begin("c")

	// Fire Done from three goroutines simultaneously.
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); a.Done(PhaseMetrics{Cost: 0.01}) }()
	go func() { defer wg.Done(); b.Done(PhaseMetrics{Cost: 0.02}) }()
	go func() { defer wg.Done(); c.Done(PhaseMetrics{Cost: 0.03}) }()
	wg.Wait()

	events := drain()
	var endCount int
	for _, ev := range events {
		if _, ok := ev.(tui.TurnEndEvent); ok {
			endCount++
		}
	}
	if endCount != 1 {
		t.Errorf("TurnEndEvent count = %d, want exactly 1", endCount)
	}
}

// --- Wait ---

func TestTracker_Wait_UnblocksOnCompletion(t *testing.T) {
	withCleanRegistry(t)
	Register(Phase{Name: "main", Order: 100})

	tracker := NewTracker(42, nil, nil, "", "")
	phase := tracker.Begin("main")

	done := make(chan struct{})
	go func() {
		tracker.Wait()
		close(done)
	}()

	// Wait should not unblock yet.
	select {
	case <-done:
		t.Fatal("Wait() unblocked before Done()")
	case <-time.After(50 * time.Millisecond):
		// expected
	}

	phase.Done(PhaseMetrics{})

	select {
	case <-done:
		// expected
	case <-time.After(time.Second):
		t.Fatal("Wait() did not unblock after Done()")
	}
}

// --- StopTyping ---

func TestTracker_StopTyping_CalledOnCompletion(t *testing.T) {
	withCleanRegistry(t)
	Register(Phase{Name: "main", Order: 100})

	stopped := make(chan struct{})
	stopFn := func() { close(stopped) }

	tracker := NewTracker(42, nil, stopFn, "", "")
	phase := tracker.Begin("main")
	phase.Done(PhaseMetrics{})

	select {
	case <-stopped:
		// expected
	case <-time.After(time.Second):
		t.Error("stopTypingFn was not called on phase completion")
	}
}

func TestTracker_StopTyping_EarlyCallIsSafe(t *testing.T) {
	withCleanRegistry(t)
	Register(Phase{Name: "main", Order: 100})

	callCount := 0
	var mu sync.Mutex
	stopFn := func() {
		mu.Lock()
		callCount++
		mu.Unlock()
	}

	tracker := NewTracker(42, nil, stopFn, "", "")
	phase := tracker.Begin("main")

	// Call StopTyping early (like the reply tool would).
	tracker.StopTyping()

	// Then complete the phase (which also tries StopTyping).
	phase.Done(PhaseMetrics{})
	time.Sleep(30 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if callCount != 1 {
		t.Errorf("stopFn called %d times, want exactly 1 (sync.Once)", callCount)
	}
}

// --- EmitToolCall ---

func TestPhaseHandle_EmitToolCall_SetsSourceAndTurnID(t *testing.T) {
	withCleanRegistry(t)
	Register(Phase{Name: "memory", Order: 200})

	bus, drain := captureBus()
	tracker := NewTracker(42, bus, nil, "", "")

	phase := tracker.Begin("memory")
	phase.EmitToolCall("save_memory", `{"fact":"test"}`, "ok: saved", false)
	phase.Done(PhaseMetrics{})

	events := drain()
	var toolEvent *tui.ToolCallEvent
	for _, ev := range events {
		if e, ok := ev.(tui.ToolCallEvent); ok {
			toolEvent = &e
		}
	}
	if toolEvent == nil {
		t.Fatal("no ToolCallEvent emitted")
	}
	if toolEvent.Source != "memory" {
		t.Errorf("Source = %q, want %q", toolEvent.Source, "memory")
	}
	if toolEvent.TurnID != 42 {
		t.Errorf("TurnID = %d, want 42", toolEvent.TurnID)
	}
	if toolEvent.ToolName != "save_memory" {
		t.Errorf("ToolName = %q, want %q", toolEvent.ToolName, "save_memory")
	}
}

// --- Nil safety ---

func TestTracker_NilBus_DoesNotPanic(t *testing.T) {
	withCleanRegistry(t)
	Register(Phase{Name: "main", Order: 100})

	// nil bus = sim mode. Should not panic.
	tracker := NewTracker(42, nil, nil, "hello", "conv-1")
	phase := tracker.Begin("main")
	phase.EmitToolCall("think", "{}", "ok", false)
	phase.Done(PhaseMetrics{Cost: 0.05})
	tracker.Wait()
}

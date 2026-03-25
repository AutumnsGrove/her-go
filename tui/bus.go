package tui

import "sync"

// Bus distributes events from producers (logger, agent, bot, sidecars) to
// all registered consumers (TUI model, file logger). Think of it like a
// message broker — Redis pub/sub or Python's blinker, but using Go channels.
//
// Thread-safe: Emit() can be called from any goroutine (agent loop, signal
// handler, sidecar scanners, etc.) without external synchronization.
type Bus struct {
	mu     sync.RWMutex
	subs   []chan Event
	closed bool
}

// NewBus creates a new event bus with no subscribers.
func NewBus() *Bus {
	return &Bus{}
}

// Subscribe returns a channel that receives all emitted events. The buffer
// size controls how many events can queue before the subscriber falls behind.
//
// In Go, buffered channels act like bounded queues — if the buffer fills up,
// Emit() will skip that subscriber rather than blocking the producer. This is
// important because we never want a slow TUI render to block the agent from
// processing messages.
//
// Typical buffer sizes:
//   - TUI: 256 (UI updates can batch)
//   - File logger: 512 (disk I/O might lag)
func (b *Bus) Subscribe(bufSize int) <-chan Event {
	ch := make(chan Event, bufSize)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		close(ch)
		return ch
	}
	b.subs = append(b.subs, ch)
	return ch
}

// Emit sends an event to all subscribers. Non-blocking — if a subscriber's
// channel buffer is full, the event is silently dropped for that subscriber.
//
// The select/default pattern here is a Go idiom for "try to send, but don't
// wait." It's like Python's queue.put_nowait() — if the queue is full, it
// just moves on instead of blocking.
func (b *Bus) Emit(e Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subs {
		select {
		case ch <- e:
		default:
			// subscriber buffer full — drop event rather than block
		}
	}
}

// Close closes all subscriber channels. This signals consumers to shut down:
// - The TUI's listenForEvents() sees channel close → returns tea.Quit
// - The file logger's range loop exits naturally
//
// Safe to call multiple times (idempotent).
func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for _, ch := range b.subs {
		close(ch)
	}
	b.subs = nil
}

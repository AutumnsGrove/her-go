// Package bot — batcher.go implements background agent batching.
//
// Instead of running the memory, mood, and introspection agents after
// every single turn, the batcher accumulates turns and fires them every
// N turns (default 3). This cuts background agent costs by ~60%.
//
// The batcher is a simple counter with an inactivity timer. Every turn
// increments the counter. When it hits the threshold, the background
// agents fire normally (they see all intermediate turns in the DB).
// If the user goes idle before the threshold, the timer flushes early
// so no turns are lost.
//
// This works because the memory agent already reads recent messages
// from the store — it sees the full conversation context regardless
// of when it runs. No transcript concatenation or agent API changes
// are needed.
package bot

import (
	"sync"
	"time"
)

// BackgroundBatcher controls when background agents fire. It tracks a
// turn counter and an inactivity timer. Background agents run when
// either the counter hits the threshold OR the timer fires (whichever
// comes first).
type BackgroundBatcher struct {
	mu        sync.Mutex
	count     int
	threshold int
	timer     *time.Timer
	timerDur  time.Duration
	flushFn   func() // called when it's time to fire background agents
	stopped   bool
}

// NewBackgroundBatcher creates a batcher with the given threshold and
// inactivity timeout.
//
// threshold: number of turns between background agent runs (default 3).
// timeout: how long to wait after the last turn before flushing anyway.
// flushFn: called when it's time to fire background agents.
func NewBackgroundBatcher(threshold int, timeout time.Duration, flushFn func()) *BackgroundBatcher {
	if threshold <= 0 {
		threshold = 3
	}
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	return &BackgroundBatcher{
		threshold: threshold,
		timerDur:  timeout,
		flushFn:   flushFn,
	}
}

// RecordTurn registers that a turn just completed. Returns true if
// background agents should fire NOW (threshold reached), false if
// they should be skipped this turn.
//
// The caller is responsible for actually launching the agents when
// this returns true. The batcher just says when.
func (b *BackgroundBatcher) RecordTurn() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.stopped {
		return true // always fire if batcher is stopped
	}

	b.count++

	// Reset the inactivity timer.
	if b.timer != nil {
		b.timer.Stop()
	}
	b.timer = time.AfterFunc(b.timerDur, func() {
		b.mu.Lock()
		shouldFlush := b.count > 0 && !b.stopped
		fn := b.flushFn
		b.count = 0
		b.mu.Unlock()

		if shouldFlush && fn != nil {
			fn()
		}
	})

	if b.count >= b.threshold {
		b.count = 0
		return true
	}
	return false
}

// Stop cancels any pending timer. Call on shutdown.
func (b *BackgroundBatcher) Stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stopped = true
	if b.timer != nil {
		b.timer.Stop()
	}
}

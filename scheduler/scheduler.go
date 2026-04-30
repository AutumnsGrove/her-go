// Package scheduler fires registered tasks at the times declared in
// their per-extension task.yaml files. It's a lean, polling-based
// executor — no cron daemons or external services — and it knows
// nothing about what tasks actually do; extensions own that via the
// Handler interface.
//
// Architecture overview:
//
//   - Extensions self-register at init() time via Register(handler).
//     Each handler owns exactly one task kind and declares its schedule
//     in a task.yaml file colocated with its Go code.
//   - At startup, New() walks the registered handlers, reads each
//     task.yaml, and upserts a row in the scheduler_tasks table.
//   - Run(ctx) spins a 30-second ticker. On each tick, it fetches
//     tasks where next_fire <= now and dispatches them to their
//     handler. Success advances next_fire to the next cron occurrence;
//     failure applies the task's retry policy.
//   - Handlers receive a *Deps bundle holding the DB store, Telegram
//     send functions, and the owner's chat ID. They return an error to
//     signal failure.
//
// See docs/plans/PLAN-mood-tracking-redesign.md for the full design,
// including how this is intended to host future extensions (reminder
// tool, persona reflection cadence, weekly digests, etc.).
package scheduler

import (
	"context"
	"fmt"
	"sync"
	"time"

	"her/logger"
	"her/memory"

	"github.com/robfig/cron/v3"
)

// TickInterval is how often the runner polls the DB for due tasks.
// 30 seconds is a reasonable trade-off: tasks fire within a 30s window
// of their cron time (fine for daily rollups and the like) while the DB
// stays cool. Exported so tests can tune it via a setter in a helper
// file if needed later.
const TickInterval = 30 * time.Second

// Scheduler owns the runner goroutine and the extension registry view
// for a single SQLite store. Construct one via New() at app startup
// and call Run(ctx) in its own goroutine.
type Scheduler struct {
	store   memory.Store
	deps    *Deps
	rootDir string
	log     *logger.Logger

	parser cron.Parser

	// mu protects the "running" flag so Run can't be called twice
	// concurrently.
	mu      sync.Mutex
	running bool
}

// New constructs a Scheduler bound to a memory store and a Deps bundle.
// It immediately loads every registered handler's task.yaml and upserts
// the scheduler_tasks rows. Call Run(ctx) separately to start the tick
// loop.
//
// rootDir is the project root — Handler.ConfigPath() paths are resolved
// relative to it. Pass the result of os.Getwd() from main, or an
// explicit project path from config.
func New(store memory.Store, deps *Deps, rootDir string) (*Scheduler, error) {
	if store == nil {
		return nil, fmt.Errorf("scheduler.New: nil store")
	}
	if deps == nil {
		return nil, fmt.Errorf("scheduler.New: nil deps")
	}

	s := &Scheduler{
		store:   store,
		deps:    deps,
		rootDir: rootDir,
		log:     logger.WithPrefix("scheduler"),
		parser: cron.NewParser(
			cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow,
		),
	}

	if err := s.loadAndUpsertAll(rootDir); err != nil {
		return nil, err
	}

	s.log.Info("scheduler initialized",
		"registered_kinds", len(registeredKinds()),
		"tick_interval", TickInterval,
	)
	return s, nil
}

// Run blocks, ticking at TickInterval, dispatching due tasks, until
// ctx is cancelled. Returns nil on a clean shutdown.
//
// Typical usage from main():
//
//	sched, err := scheduler.New(store, deps, rootDir)
//	if err != nil { ... }
//	go func() {
//	    if err := sched.Run(ctx); err != nil {
//	        log.Error("scheduler stopped", "err", err)
//	    }
//	}()
//
// Go note: the goroutine + context pattern is standard for long-lived
// background services in Go — same role as a supervised thread in
// Python, but with cooperative cancellation via the ctx.Done() channel.
func (s *Scheduler) Run(ctx context.Context) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return fmt.Errorf("scheduler.Run: already running")
	}
	s.running = true
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()

	// Fire a tick immediately so tasks that went due during downtime
	// don't have to wait 30 seconds.
	s.tick(ctx)

	ticker := time.NewTicker(TickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.log.Info("scheduler shutting down")
			return nil
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

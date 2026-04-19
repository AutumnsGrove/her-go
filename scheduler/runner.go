package scheduler

import (
	"context"
	"fmt"
	"time"

	"her/memory"
)

// tick fetches every due task and dispatches it. Called once per
// ticker interval from Run(). A slow handler delays subsequent handlers
// on the same tick, which is fine for our scale — we don't have
// high-frequency tasks. If that changes, we'd spawn a bounded worker
// pool here.
func (s *Scheduler) tick(ctx context.Context) {
	if err := ctx.Err(); err != nil {
		return
	}

	now := time.Now()
	tasks, err := s.store.DueSchedulerTasks(now)
	if err != nil {
		s.log.Error("fetching due tasks", "err", err)
		return
	}
	if len(tasks) == 0 {
		return
	}

	s.log.Debug("scheduler tick", "due", len(tasks))

	for _, t := range tasks {
		if ctx.Err() != nil {
			return
		}
		s.dispatch(ctx, &t)
	}
}

// dispatch runs a single due task. It looks up the registered handler,
// invokes Execute, and applies success/failure bookkeeping. Panics from
// handlers are recovered and treated as errors — a buggy extension
// shouldn't crash the scheduler.
func (s *Scheduler) dispatch(ctx context.Context, t *memory.SchedulerTask) {
	h := lookup(t.Kind)
	if h == nil {
		// Handler was deleted or renamed but the row survived. Nudge
		// next_fire forward by a day so we don't spin on it; leave the
		// row in place for human inspection.
		s.log.Warn("no handler registered for kind; deferring 24h",
			"kind", t.Kind, "id", t.ID,
		)
		_ = s.store.MarkSchedulerFailure(
			t.ID,
			time.Now().Add(24*time.Hour),
			"no handler registered",
			t.AttemptCount,
		)
		return
	}

	err := runHandler(ctx, h, t.Payload, s.deps)

	if err == nil {
		next, nextErr := s.nextFire(t, time.Now())
		if nextErr != nil {
			s.log.Error("computing next fire after success",
				"kind", t.Kind, "err", nextErr,
			)
			// park the task a day out so it doesn't spin
			next = time.Now().Add(24 * time.Hour)
		}
		if err := s.store.MarkSchedulerSuccess(t.ID, next); err != nil {
			s.log.Error("marking success", "kind", t.Kind, "err", err)
		} else {
			s.log.Info("scheduler task fired",
				"kind", t.Kind,
				"next_fire", next.Format(time.RFC3339),
			)
		}
		return
	}

	// Failure path — apply retry policy.
	attempts := t.AttemptCount + 1
	s.log.Error("scheduler task failed",
		"kind", t.Kind, "attempt", attempts, "err", err,
	)

	retry := RetryConfig{
		MaxAttempts: t.RetryMaxAttempts,
		Backoff:     t.RetryBackoff,
		InitialWait: t.RetryInitialWait,
	}

	var nextFire time.Time
	if attempts < retry.MaxAttempts {
		wait := retry.NextWait(attempts)
		if wait <= 0 {
			// "none" backoff — retry on next tick.
			wait = TickInterval
		}
		nextFire = time.Now().Add(wait)
	} else {
		// Exhausted retries — skip to next scheduled fire.
		attempts = 0
		next, nextErr := s.nextFire(t, time.Now())
		if nextErr != nil {
			s.log.Error("computing next fire after exhausted retries",
				"kind", t.Kind, "err", nextErr,
			)
			next = time.Now().Add(24 * time.Hour)
		}
		nextFire = next
	}

	if markErr := s.store.MarkSchedulerFailure(t.ID, nextFire, err.Error(), attempts); markErr != nil {
		s.log.Error("marking failure", "kind", t.Kind, "err", markErr)
	}
}

// nextFire computes the next cron occurrence after `after`. For
// one-shot tasks (empty CronExpr) it returns an error — callers should
// delete the row instead; no one-shot support yet (see Scheduler.Delete
// if that changes later).
func (s *Scheduler) nextFire(t *memory.SchedulerTask, after time.Time) (time.Time, error) {
	if t.CronExpr == "" {
		return time.Time{}, fmt.Errorf("no cron expression on task %d (one-shot not supported yet)", t.ID)
	}
	sched, err := s.parser.Parse(t.CronExpr)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing cron %q: %w", t.CronExpr, err)
	}
	return sched.Next(after), nil
}

// runHandler wraps handler.Execute with a panic recovery so a buggy
// extension can't crash the scheduler runner. Any panic is converted
// to an error.
func runHandler(ctx context.Context, h Handler, payload []byte, deps *Deps) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handler panic: %v", r)
		}
	}()
	return h.Execute(ctx, payload, deps)
}

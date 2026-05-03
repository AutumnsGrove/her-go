// Package retry provides a unified retry-with-backoff mechanism.
//
// Instead of scattering ad-hoc retry loops across the codebase, callers
// use retry.Do with a Config describing the backoff strategy. This keeps
// the retry policy visible and consistent.
//
// This is Go's equivalent of Python's tenacity or JavaScript's p-retry —
// but as a simple function rather than a decorator, because Go favors
// explicit control flow over magic.
package retry

import (
	"context"
	"fmt"
	"time"

	"her/logger"
)

var log = logger.WithPrefix("retry")

// Backoff selects the delay strategy between attempts.
type Backoff int

const (
	// Exponential doubles the wait each attempt: InitialWait * 2^(attempt-1).
	// Attempt 1 = InitialWait, attempt 2 = 2x, attempt 3 = 4x, etc.
	Exponential Backoff = iota

	// Linear increases wait proportionally: InitialWait * attempt.
	// Attempt 1 = InitialWait, attempt 2 = 2x, attempt 3 = 3x, etc.
	Linear
)

// Config controls the retry behavior.
type Config struct {
	// MaxAttempts is the total number of tries (including the first).
	// Must be >= 1. A value of 1 means "try once, no retry".
	MaxAttempts int

	// Backoff selects exponential or linear delay growth.
	Backoff Backoff

	// InitialWait is the base delay. First retry waits this long;
	// subsequent retries scale it according to the Backoff strategy.
	InitialWait time.Duration

	// IsRetriable, if non-nil, is called on each error to decide whether
	// to retry. Return false to bail immediately (the error is permanent).
	// When nil, all errors are considered retriable.
	IsRetriable func(error) bool
}

// Do executes fn up to cfg.MaxAttempts times, sleeping between failures.
// It respects context cancellation — if ctx is done, it returns ctx.Err()
// without further attempts.
//
// Returns nil on the first successful call, or the last error if all
// attempts fail (or if IsRetriable returns false).
func Do(ctx context.Context, cfg Config, fn func() error) error {
	if cfg.MaxAttempts < 1 {
		cfg.MaxAttempts = 1
	}

	var lastErr error
	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		// Check context before each attempt.
		if ctx.Err() != nil {
			if lastErr != nil {
				return lastErr
			}
			return ctx.Err()
		}

		lastErr = fn()
		if lastErr == nil {
			return nil
		}

		// Check if this error is worth retrying.
		if cfg.IsRetriable != nil && !cfg.IsRetriable(lastErr) {
			return lastErr
		}

		// Don't sleep after the final attempt.
		if attempt == cfg.MaxAttempts {
			break
		}

		wait := backoffDuration(cfg.Backoff, cfg.InitialWait, attempt)
		log.Warn("attempt failed, retrying",
			"attempt", attempt,
			"max", cfg.MaxAttempts,
			"backoff", wait,
			"err", lastErr,
		)

		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return lastErr
		}
	}

	return fmt.Errorf("after %d attempts: %w", cfg.MaxAttempts, lastErr)
}

// backoffDuration calculates the sleep duration for a given attempt.
// attempt is 1-indexed (first retry = attempt 1).
func backoffDuration(strategy Backoff, initial time.Duration, attempt int) time.Duration {
	if initial <= 0 {
		return 0
	}
	switch strategy {
	case Linear:
		return initial * time.Duration(attempt)
	case Exponential:
		return initial << (attempt - 1)
	default:
		return initial
	}
}

package retry

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDo_SucceedsFirstAttempt(t *testing.T) {
	calls := 0
	err := Do(context.Background(), Config{
		MaxAttempts: 3,
		Backoff:     Exponential,
		InitialWait: 10 * time.Millisecond,
	}, func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call, got %d", calls)
	}
}

func TestDo_RetriesThenSucceeds(t *testing.T) {
	calls := 0
	err := Do(context.Background(), Config{
		MaxAttempts: 3,
		Backoff:     Exponential,
		InitialWait: 10 * time.Millisecond,
	}, func() error {
		calls++
		if calls < 3 {
			return errors.New("transient")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 calls, got %d", calls)
	}
}

func TestDo_ExhaustsAttempts(t *testing.T) {
	sentinel := errors.New("persistent failure")
	err := Do(context.Background(), Config{
		MaxAttempts: 3,
		Backoff:     Exponential,
		InitialWait: 10 * time.Millisecond,
	}, func() error {
		return sentinel
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped sentinel, got: %v", err)
	}
}

func TestDo_IsRetriable_BailsEarly(t *testing.T) {
	permanent := errors.New("permanent")
	calls := 0
	err := Do(context.Background(), Config{
		MaxAttempts: 5,
		Backoff:     Exponential,
		InitialWait: 10 * time.Millisecond,
		IsRetriable: func(err error) bool {
			return !errors.Is(err, permanent)
		},
	}, func() error {
		calls++
		return permanent
	})
	if calls != 1 {
		t.Fatalf("expected 1 call (bail on non-retriable), got %d", calls)
	}
	if !errors.Is(err, permanent) {
		t.Fatalf("expected permanent error, got: %v", err)
	}
}

func TestDo_RespectsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := Do(ctx, Config{
		MaxAttempts: 10,
		Backoff:     Exponential,
		InitialWait: 1 * time.Second,
	}, func() error {
		calls++
		cancel()
		return errors.New("will be cancelled")
	})
	if calls != 1 {
		t.Fatalf("expected 1 call before cancel, got %d", calls)
	}
	if err == nil {
		t.Fatal("expected error after cancel")
	}
}

// TestDo_ContextCancellation verifies that cancelling the context during
// the backoff sleep causes Do to return quickly with an error that wraps
// "context cancelled during retry backoff". This tests the select branch:
//
//	case <-ctx.Done():
//	    timer.Stop()
//	    return fmt.Errorf("%w (context cancelled during retry backoff)", lastErr)
//
// Without this fix, a cancelled context would still wait out the full
// backoff sleep before checking cancellation on the next iteration —
// potentially stalling for minutes with a long InitialWait.
func TestDo_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	calls := 0
	sentinel := errors.New("transient failure")

	// Use a long backoff so the test would time out (~5s) if the
	// context cancellation isn't detected during the sleep.
	done := make(chan error, 1)
	go func() {
		done <- Do(ctx, Config{
			MaxAttempts: 5,
			Backoff:     Exponential,
			InitialWait: 5 * time.Second,
		}, func() error {
			calls++
			// Cancel the context after the first failure so Do
			// enters the backoff sleep with a cancelled context.
			if calls == 1 {
				cancel()
			}
			return sentinel
		})
	}()

	// Do must return well within 1 second — not after the full 5s sleep.
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		// The error must mention "context cancelled during retry backoff"
		// so callers can distinguish a graceful shutdown from an exhausted
		// retry budget.
		if !errors.Is(err, sentinel) {
			t.Errorf("expected wrapped sentinel error, got: %v", err)
		}
		wantSubstr := "context cancelled during retry backoff"
		if !strings.Contains(err.Error(), wantSubstr) {
			t.Errorf("error message %q does not contain %q", err.Error(), wantSubstr)
		}
	case <-time.After(1 * time.Second):
		cancel() // clean up
		t.Fatal("Do did not return within 1s after context cancellation — backoff sleep was not interrupted")
	}

	if calls != 1 {
		t.Errorf("fn called %d times, want 1 (cancel after first failure)", calls)
	}
}


func TestDo_LinearBackoff(t *testing.T) {
	start := time.Now()
	calls := 0
	Do(context.Background(), Config{
		MaxAttempts: 3,
		Backoff:     Linear,
		InitialWait: 50 * time.Millisecond,
	}, func() error {
		calls++
		return errors.New("fail")
	})
	elapsed := time.Since(start)
	// Linear: 50ms (attempt 1) + 100ms (attempt 2) = 150ms minimum
	if elapsed < 140*time.Millisecond {
		t.Fatalf("expected >= 140ms for linear backoff, got %v", elapsed)
	}
}

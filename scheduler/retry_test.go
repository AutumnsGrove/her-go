package scheduler

import (
	"testing"
	"time"
)

// TestRetryConfig_Valid verifies that Valid() only accepts the four
// backoff strings the runner actually knows how to execute. A bad value
// here would mean loader.go accepts nonsense YAML and the runner
// silently no-ops on retry.
func TestRetryConfig_Valid(t *testing.T) {
	tests := []struct {
		name    string
		backoff string
		want    bool
	}{
		{"empty string treated as none", "", true},
		{"explicit none", "none", true},
		{"linear", "linear", true},
		{"exponential", "exponential", true},
		{"typo is rejected", "exponetial", false},
		{"capitalized is rejected", "Linear", false},
		{"garbage is rejected", "fibonacci", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := RetryConfig{Backoff: tc.backoff}
			if got := r.Valid(); got != tc.want {
				t.Errorf("Valid(backoff=%q) = %v, want %v", tc.backoff, got, tc.want)
			}
		})
	}
}

// TestRetryConfig_NextWait_Linear checks the linear backoff formula
// for the first few attempts. Formula: InitialWait * attempt.
func TestRetryConfig_NextWait_Linear(t *testing.T) {
	r := RetryConfig{
		MaxAttempts: 10,
		Backoff:     "linear",
		InitialWait: 60 * time.Second,
	}
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 60 * time.Second},
		{2, 120 * time.Second},
		{3, 180 * time.Second},
	}
	for _, tc := range tests {
		if got := r.NextWait(tc.attempt); got != tc.want {
			t.Errorf("NextWait(attempt=%d) = %v, want %v", tc.attempt, got, tc.want)
		}
	}
}

// TestRetryConfig_NextWait_Exponential checks that each attempt doubles
// the previous wait: 1x, 2x, 4x, 8x of InitialWait. Formula in code:
// InitialWait << (attempt-1).
func TestRetryConfig_NextWait_Exponential(t *testing.T) {
	r := RetryConfig{
		MaxAttempts: 10,
		Backoff:     "exponential",
		InitialWait: 60 * time.Second,
	}
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 60 * time.Second},        // 1x
		{2, 120 * time.Second},       // 2x
		{3, 240 * time.Second},       // 4x
		{4, 480 * time.Second},       // 8x
	}
	for _, tc := range tests {
		if got := r.NextWait(tc.attempt); got != tc.want {
			t.Errorf("NextWait(attempt=%d) = %v, want %v", tc.attempt, got, tc.want)
		}
	}
}

// TestRetryConfig_NextWait_None returns zero so the runner knows to
// fall back to "retry on next tick" behavior.
func TestRetryConfig_NextWait_None(t *testing.T) {
	r := RetryConfig{
		MaxAttempts: 5,
		Backoff:     "none",
		InitialWait: 60 * time.Second,
	}
	if got := r.NextWait(1); got != 0 {
		t.Errorf("NextWait(none) = %v, want 0", got)
	}
}

// TestRetryConfig_NextWait_ZeroInitialWait guards against misconfigured
// task.yaml files that set a backoff strategy but forget initial_wait.
// Without this guard, exponential backoff would << 0 = 0 and retry
// immediately in a tight loop.
func TestRetryConfig_NextWait_ZeroInitialWait(t *testing.T) {
	r := RetryConfig{
		MaxAttempts: 3,
		Backoff:     "exponential",
		InitialWait: 0,
	}
	if got := r.NextWait(1); got != 0 {
		t.Errorf("NextWait(exponential, InitialWait=0) = %v, want 0", got)
	}
}

// TestRetryConfig_NextWait_InvalidAttempt treats attempt < 1 as a
// programmer error and returns 0 rather than crashing. Keeps the runner
// resilient if it ever passes attempt=0 through.
func TestRetryConfig_NextWait_InvalidAttempt(t *testing.T) {
	r := RetryConfig{
		MaxAttempts: 5,
		Backoff:     "linear",
		InitialWait: 60 * time.Second,
	}
	if got := r.NextWait(0); got != 0 {
		t.Errorf("NextWait(attempt=0) = %v, want 0", got)
	}
}

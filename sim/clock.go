package sim

import (
	"sync"
	"time"
)

// Clock is a minimal time source the sim uses in place of `time.Now()`.
// Production code keeps calling time.Now() directly — only the sim
// injects one. Keeping the surface tiny (Now + NewTimer equivalent via
// Advance) prevents the sim from coupling to the standard library's
// full time API, which has its own quirks under fakes.
//
// Go note: this is an interface with two implementations — same idea as
// Python's abstract base class, but satisfied implicitly. RealClock is
// what you'd use in production code if Now() ever needs to be
// injectable; FakeClock is what scenarios use to jump to specific times.
type Clock interface {
	Now() time.Time
}

// RealClock returns wall-clock time. Sims may use it too when the test
// actually wants real-time behavior (rare — prefer FakeClock).
type RealClock struct{}

// Now returns the current wall-clock time.
func (RealClock) Now() time.Time { return time.Now() }

// FakeClock is a controllable time source. Start it at a fixed time,
// then move it forward with Advance() in the middle of a scenario to
// make cron tasks come due.
type FakeClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewFakeClock starts the clock at the given time.
func NewFakeClock(start time.Time) *FakeClock {
	return &FakeClock{now: start}
}

// Now returns the current simulated time.
func (fc *FakeClock) Now() time.Time {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.now
}

// Advance moves the clock forward by d. Passing a negative d is
// allowed but typically indicates a test bug — scenarios model a
// monotonically-moving day.
func (fc *FakeClock) Advance(d time.Duration) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.now = fc.now.Add(d)
}

// SetTo jumps the clock to an absolute wall-clock time. Useful for
// scenarios that want to land at "exactly 21:00 on this date" rather
// than computing a delta from the current sim time.
func (fc *FakeClock) SetTo(t time.Time) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.now = t
}

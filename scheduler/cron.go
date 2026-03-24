// cron.go — Cron expression parsing and next-run computation.
//
// This wraps robfig/cron/v3 for exactly two purposes:
//   1. Compute the next fire time for a cron expression
//   2. Validate a cron expression before storing it
//
// These are pure functions — no state, no side effects. The scheduler
// calls NextRun after each task execution to compute the next_run column,
// and the agent tools call ValidateCron when creating recurring tasks.
//
// Think of robfig/cron as a PARSER, not a scheduler. It knows how to
// read "0 8 * * *" and answer "when does this fire next?" — that's it.
// Our actual scheduling loop is in scheduler.go (a simple time.Ticker
// polling the database). All state lives in SQLite, so it survives restarts.
package scheduler

import (
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
)

// cronParser is a package-level parser for standard 5-field cron
// expressions: minute, hour, day-of-month, month, day-of-week.
//
// The Descriptor flag adds support for shortcuts like @every, @daily,
// @hourly — so "0 8 * * *" and "@daily" both work.
//
// Go package-level vars are initialized once at startup (before main
// runs). Same idea as a module-level constant in Python — create once,
// reuse everywhere. This is safe for concurrent use.
var cronParser = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
)

// NextRun computes the next time a cron expression fires after `after`,
// evaluated in the given timezone. Returns the result in UTC for
// consistent database storage.
//
// Example:
//
//	next, err := NextRun("0 8 * * *", time.Now(), nyc)
//	// next = tomorrow at 8:00 AM Eastern, converted to UTC
//
// The timezone matters because "0 8 * * *" means "8am" — but 8am WHERE?
// We parse relative to the user's configured timezone, then convert to
// UTC for storage. When displaying to the user, convert back to local.
func NextRun(cronExpr string, after time.Time, loc *time.Location) (time.Time, error) {
	sched, err := cronParser.Parse(cronExpr)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid cron expression %q: %w", cronExpr, err)
	}

	// sched.Next() computes the next fire time after the given time.
	// We convert `after` to the target timezone first so the cron
	// expression is evaluated in local time (e.g., "0 8 * * *" = 8am
	// in the user's timezone, not 8am UTC).
	next := sched.Next(after.In(loc))
	return next.UTC(), nil
}

// ValidateCron checks whether a cron expression is syntactically valid.
// Used by agent tools to reject bad expressions before creating tasks,
// so we get a clean error message instead of a database row that can
// never fire.
func ValidateCron(cronExpr string) error {
	_, err := cronParser.Parse(cronExpr)
	if err != nil {
		return fmt.Errorf("invalid cron expression %q: %w", cronExpr, err)
	}
	return nil
}

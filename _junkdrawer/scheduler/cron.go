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

// DescribeCron converts a cron expression to a human-readable string
// so the agent (and user) don't have to parse cron syntax themselves.
// Trinity was misreading "30 9 * * *" as 3pm because it couldn't parse
// cron — this fixes that by doing the conversion server-side.
//
// Handles common patterns; falls back to the raw expression for complex ones.
func DescribeCron(cronExpr string) string {
	// Handle @descriptors first.
	switch cronExpr {
	case "@daily", "@midnight":
		return "daily at midnight"
	case "@hourly":
		return "every hour"
	case "@weekly":
		return "weekly (Sunday midnight)"
	case "@monthly":
		return "monthly (1st at midnight)"
	case "@yearly", "@annually":
		return "yearly (Jan 1 at midnight)"
	}

	// Handle @every expressions.
	if len(cronExpr) > 6 && cronExpr[:6] == "@every" {
		return "every " + cronExpr[7:]
	}

	// Parse standard 5-field: minute hour dom month dow
	fields := splitFields(cronExpr)
	if len(fields) != 5 {
		return cronExpr // can't parse, return raw
	}

	minute, hour, dom, month, dow := fields[0], fields[1], fields[2], fields[3], fields[4]

	// Build time string
	timeStr := ""
	if hour != "*" && minute != "*" {
		// Specific time like "30 9" → "9:30 AM"
		h := 0
		m := 0
		fmt.Sscanf(hour, "%d", &h)
		fmt.Sscanf(minute, "%d", &m)
		ampm := "AM"
		displayH := h
		if h >= 12 {
			ampm = "PM"
			if h > 12 {
				displayH = h - 12
			}
		}
		if h == 0 {
			displayH = 12
		}
		timeStr = fmt.Sprintf("%d:%02d %s", displayH, m, ampm)
	} else if hour != "*" {
		h := 0
		fmt.Sscanf(hour, "%d", &h)
		ampm := "AM"
		displayH := h
		if h >= 12 {
			ampm = "PM"
			if h > 12 {
				displayH = h - 12
			}
		}
		if h == 0 {
			displayH = 12
		}
		timeStr = fmt.Sprintf("%d:00 %s", displayH, ampm)
	}

	// Build schedule string
	dayStr := ""
	if dow != "*" {
		dayNames := map[string]string{
			"0": "Sun", "1": "Mon", "2": "Tue", "3": "Wed",
			"4": "Thu", "5": "Fri", "6": "Sat", "7": "Sun",
			"1-5": "weekdays", "0,6": "weekends",
		}
		if name, ok := dayNames[dow]; ok {
			dayStr = name
		} else {
			dayStr = "days " + dow
		}
	}

	// Combine
	if dom == "*" && month == "*" && dow == "*" && timeStr != "" {
		return "daily at " + timeStr
	}
	if dow != "*" && dom == "*" && month == "*" && timeStr != "" {
		return dayStr + " at " + timeStr
	}
	if timeStr != "" {
		return "at " + timeStr
	}

	return cronExpr // complex, return raw
}

// splitFields splits a cron expression into its 5 fields.
func splitFields(expr string) []string {
	var fields []string
	field := ""
	for _, c := range expr {
		if c == ' ' || c == '\t' {
			if field != "" {
				fields = append(fields, field)
				field = ""
			}
		} else {
			field += string(c)
		}
	}
	if field != "" {
		fields = append(fields, field)
	}
	return fields
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

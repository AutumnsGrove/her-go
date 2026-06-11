// cron.go — Exported cron helpers for tool handlers and display.
//
// These are pure functions wrapping robfig/cron/v3. Tools call
// ValidateCron + NextRun when creating schedules; list_schedules
// calls DescribeCron for human-readable display.
package scheduler

import (
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
)

// userParser includes the Descriptor flag so users can write @daily,
// @hourly, etc. in addition to standard 5-field expressions.
var userParser = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
)

// ValidateCron checks whether a cron expression is syntactically valid.
func ValidateCron(cronExpr string) error {
	_, err := userParser.Parse(cronExpr)
	if err != nil {
		return fmt.Errorf("invalid cron expression %q: %w", cronExpr, err)
	}
	return nil
}

// NextRun computes the next fire time after `after`, evaluated in the
// given timezone. Returns the result in UTC for database storage.
func NextRun(cronExpr string, after time.Time, loc *time.Location) (time.Time, error) {
	sched, err := userParser.Parse(cronExpr)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid cron expression %q: %w", cronExpr, err)
	}
	next := sched.Next(after.In(loc))
	return next.UTC(), nil
}

// DescribeCron converts a cron expression to a human-readable string.
func DescribeCron(cronExpr string) string {
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

	if len(cronExpr) > 6 && cronExpr[:6] == "@every" {
		return "every " + cronExpr[7:]
	}

	fields := splitCronFields(cronExpr)
	if len(fields) != 5 {
		return cronExpr
	}

	minute, hour, dom, month, dow := fields[0], fields[1], fields[2], fields[3], fields[4]

	timeStr := ""
	if hour != "*" && minute != "*" {
		h, m := 0, 0
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

	if dom == "*" && month == "*" && dow == "*" && timeStr != "" {
		return "daily at " + timeStr
	}
	if dow != "*" && dom == "*" && month == "*" && timeStr != "" {
		return dayStr + " at " + timeStr
	}
	if timeStr != "" {
		return "at " + timeStr
	}

	return cronExpr
}

func splitCronFields(expr string) []string {
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

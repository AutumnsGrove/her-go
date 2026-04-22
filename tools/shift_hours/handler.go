package shift_hours

import (
	"encoding/json"
	"fmt"
	"time"

	"her/tools"
)

func init() {
	tools.Register("shift_hours", Handle)
}

// Handle computes total hours worked over a time period, broken down by job.
// It queries shift events from the DB, parses time chit values from notes,
// and returns per-job and overall totals — so the agent never has to do math.
//
// When a shift has a time chit (actual hours from the receipt), that's used.
// Otherwise, scheduled hours (end - start) are the fallback. This means you
// get accurate totals even for shifts where the time chit hasn't been logged yet.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		Period string `json:"period,omitempty"`
		Start  string `json:"start,omitempty"`
		End    string `json:"end,omitempty"`
		Job    string `json:"job,omitempty"`
	}

	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error: failed to parse arguments: %v", err)
	}

	// Default period to "month" if not specified
	if args.Period == "" {
		args.Period = "month"
	}

	// Resolve period to start/end timestamps. Uses the configured timezone
	// so "this week" means the user's local week, not UTC.
	start, end, periodLabel, err := resolvePeriod(args.Period, args.Start, args.End, ctx.Cfg.Calendar.DefaultTimezone)
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}

	// Query shift events from the DB
	events, err := ctx.Store.ListShiftEvents(start, end, args.Job)
	if err != nil {
		return fmt.Sprintf("error: failed to query shifts: %v", err)
	}

	// Aggregate hours per job. Each entry tracks total minutes and shift count.
	// In Python you'd use a defaultdict(lambda: {"mins": 0, "count": 0}).
	// In Go, we use a map with a struct value — same idea, different syntax.
	type jobTotal struct {
		Minutes int `json:"minutes"`
		Count   int `json:"count"`
	}
	byJob := make(map[string]*jobTotal)
	overallMinutes := 0

	for _, e := range events {
		// Parse shift metadata from notes to get time chit
		sn := tools.ParseShiftNotes(e.Notes)

		var minutes int
		if sn.TimeChit != "" {
			// Use actual hours from time chit receipt
			if parsed, ok := tools.ParseTimeChit(sn.TimeChit); ok {
				minutes = parsed
			} else {
				// Time chit exists but doesn't parse — fall back to scheduled
				minutes = int(e.End.Sub(e.Start).Minutes())
			}
		} else {
			// No time chit yet — use scheduled hours (end - start)
			minutes = int(e.End.Sub(e.Start).Minutes())
		}

		if byJob[e.Job] == nil {
			byJob[e.Job] = &jobTotal{}
		}
		byJob[e.Job].Minutes += minutes
		byJob[e.Job].Count++
		overallMinutes += minutes
	}

	// Build the response
	type jobEntry struct {
		Job    string `json:"job"`
		Shifts int    `json:"shifts"`
		Hours  string `json:"hours"`
	}

	jobEntries := make([]jobEntry, 0, len(byJob))
	for job, total := range byJob {
		jobEntries = append(jobEntries, jobEntry{
			Job:    job,
			Shifts: total.Count,
			Hours:  tools.FormatMinutes(total.Minutes),
		})
	}

	result := map[string]any{
		"period": periodLabel,
		"by_job": jobEntries,
		"total": map[string]any{
			"shifts": len(events),
			"hours":  tools.FormatMinutes(overallMinutes),
		},
	}

	resultJSON, err := json.Marshal(result)
	if err != nil {
		return fmt.Sprintf("error: failed to marshal result: %v", err)
	}

	return string(resultJSON)
}

// resolvePeriod converts a named period ("week", "month", "year") or custom
// range into ISO 8601 start/end strings and a human-readable label. The
// timezone param ensures "this week" is relative to the user's local time.
//
// time.LoadLocation is Go's equivalent of Python's pytz.timezone() — it loads
// IANA timezone data and returns a *time.Location for use with time.Now().In().
func resolvePeriod(period, customStart, customEnd, timezone string) (start, end, label string, err error) {
	// Load timezone — fall back to local if not configured or invalid
	loc, locErr := time.LoadLocation(timezone)
	if locErr != nil {
		loc = time.Now().Location()
	}

	now := time.Now().In(loc)
	const iso = "2006-01-02T15:04:05"

	switch period {
	case "week":
		// Go's time.Weekday() returns 0=Sunday. We want Monday as start of week.
		// This offset math finds the most recent Monday.
		weekday := int(now.Weekday())
		if weekday == 0 {
			weekday = 7 // Sunday → 7
		}
		monday := now.AddDate(0, 0, -(weekday - 1))
		weekStart := time.Date(monday.Year(), monday.Month(), monday.Day(), 0, 0, 0, 0, loc)
		weekEnd := weekStart.AddDate(0, 0, 7).Add(-time.Second)

		start = weekStart.Format(iso)
		end = weekEnd.Format(iso)
		label = fmt.Sprintf("%s – %s", weekStart.Format("Jan 2"), weekEnd.Format("Jan 2, 2006"))

	case "month":
		monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
		monthEnd := monthStart.AddDate(0, 1, 0).Add(-time.Second)

		start = monthStart.Format(iso)
		end = monthEnd.Format(iso)
		label = now.Format("January 2006")

	case "year":
		yearStart := time.Date(now.Year(), 1, 1, 0, 0, 0, 0, loc)
		yearEnd := time.Date(now.Year(), 12, 31, 23, 59, 59, 0, loc)

		start = yearStart.Format(iso)
		end = yearEnd.Format(iso)
		label = fmt.Sprintf("%d", now.Year())

	case "custom":
		if customStart == "" || customEnd == "" {
			return "", "", "", fmt.Errorf("start and end are required when period is 'custom'")
		}
		start = customStart
		end = customEnd
		// Try to parse for a readable label, fall back to raw strings
		if s, err := time.Parse(iso, customStart); err == nil {
			if e, err := time.Parse(iso, customEnd); err == nil {
				label = fmt.Sprintf("%s – %s", s.Format("Jan 2"), e.Format("Jan 2, 2006"))
			}
		}
		if label == "" {
			label = fmt.Sprintf("%s – %s", customStart, customEnd)
		}

	default:
		return "", "", "", fmt.Errorf("unknown period %q (use week, month, year, or custom)", period)
	}

	return start, end, label, nil
}

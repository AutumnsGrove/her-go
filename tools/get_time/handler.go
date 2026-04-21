package get_time

import (
	"encoding/json"
	"fmt"
	"time"

	"her/tools"
)

func init() {
	tools.Register("get_time", Handle)
}

// Handle returns the current date and time in the configured timezone.
// No parameters required — just returns the current time with multiple formats
// for the agent to use in calculations and scheduling.
func Handle(argsJSON string, ctx *tools.Context) string {
	// Load timezone from config (Calendar.DefaultTimezone)
	// Fall back to system timezone if not configured
	loc := time.Local
	if ctx.Config.Calendar.DefaultTimezone != "" {
		loadedLoc, err := time.LoadLocation(ctx.Config.Calendar.DefaultTimezone)
		if err != nil {
			// Invalid timezone in config — warn but continue with system timezone
			return fmt.Sprintf("error: invalid timezone %q in config: %v", ctx.Config.Calendar.DefaultTimezone, err)
		}
		loc = loadedLoc
	}

	// Get current time in the configured timezone
	now := time.Now().In(loc)

	// Format multiple representations
	result := map[string]string{
		"iso":         now.Format(time.RFC3339),                      // 2026-04-21T14:32:07-04:00
		"human":       now.Format("Monday, January 2, 2006 3:04 PM"), // Monday, April 21, 2026 2:32 PM
		"day_of_week": now.Format("Monday"),                          // Monday
		"timezone":    loc.String(),                                  // America/New_York or Local
	}

	// Include timezone abbreviation if available (EDT, PST, etc.)
	zone, _ := now.Zone()
	result["human"] += " " + zone

	// Marshal to JSON
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return fmt.Sprintf("error: failed to marshal result: %v", err)
	}

	return string(resultJSON)
}

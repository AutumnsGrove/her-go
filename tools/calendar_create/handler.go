package calendar_create

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"her/calendar"
	"her/tools"
)

func init() {
	tools.Register("calendar_create", Handle)
}

// Handle creates one or more calendar events (atomic).
func Handle(argsJSON string, ctx *tools.Context) string {
	// Parse arguments
	var args struct {
		Events   []map[string]any `json:"events"`
		Calendar string           `json:"calendar,omitempty"`
	}

	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error: failed to parse arguments: %v", err)
	}

	// Validate we have events
	if len(args.Events) == 0 {
		return "error: no events provided"
	}

	// Use configured calendar if not overridden
	calendarName := args.Calendar
	if calendarName == "" {
		calendarName = ctx.Config.Calendar.CalendarName
	}

	// Build bridge request
	req := calendar.Request{
		Command:  "create",
		Calendar: calendarName,
		Args: map[string]any{
			"events": args.Events,
		},
	}

	// Initialize bridge
	bridge := calendar.NewBridge(ctx.Config, ctx.Logger)

	// Call with timeout (generous for batch creates)
	callCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := bridge.Call(callCtx, req)
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}

	// Format response as JSON for the agent
	resultJSON, err := json.Marshal(resp.Result)
	if err != nil {
		return fmt.Sprintf("error: failed to marshal result: %v", err)
	}

	return string(resultJSON)
}

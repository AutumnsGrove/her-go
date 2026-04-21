package calendar_list

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"her/calendar"
	"her/tools"

	"github.com/charmbracelet/log"
)

func init() {
	tools.Register("calendar_list", Handle)
}

// Handle lists calendar events in a date range.
func Handle(argsJSON string, ctx *tools.Context) string {
	// Parse arguments
	var args struct {
		Start    string `json:"start"`
		End      string `json:"end"`
		Calendar string `json:"calendar,omitempty"`
	}

	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error: failed to parse arguments: %v", err)
	}

	// Use configured calendar if not overridden
	calendarName := args.Calendar
	if calendarName == "" {
		calendarName = ctx.Cfg.Calendar.CalendarName
	}

	// Build bridge request
	req := calendar.Request{
		Command:  "list",
		Calendar: calendarName,
		Args: map[string]any{
			"start": args.Start,
			"end":   args.End,
		},
	}

	// Initialize bridge
	logger := log.Default()
	bridge := calendar.NewBridge(ctx.Cfg, logger)

	// Call with timeout
	callCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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

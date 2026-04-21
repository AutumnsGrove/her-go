package calendar_create

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
	tools.Register("calendar_create", Handle)
}

// Handle creates one or more calendar events (atomic).
// Each event can optionally specify a calendar; otherwise uses default_calendar.
func Handle(argsJSON string, ctx *tools.Context) string {
	// Parse arguments
	var args struct {
		Events []map[string]any `json:"events"`
	}

	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error: failed to parse arguments: %v", err)
	}

	// Validate we have events
	if len(args.Events) == 0 {
		return "error: no events provided"
	}

	// For each event, set default calendar if not specified
	for i := range args.Events {
		if _, hasCalendar := args.Events[i]["calendar"]; !hasCalendar {
			args.Events[i]["calendar"] = ctx.Cfg.Calendar.DefaultCalendar
		}
	}

	// Build bridge request
	req := calendar.Request{
		Command:  "create",
		Calendar: ctx.Cfg.Calendar.DefaultCalendar, // Swift uses this as fallback
		Args: map[string]any{
			"events": args.Events,
		},
	}

	// Initialize bridge
	logger := log.Default()
	bridge := calendar.NewCLIBridge(ctx.Cfg, logger)

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

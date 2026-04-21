package calendar_list

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"her/calendar"
	"her/tools"

	"github.com/charmbracelet/log"
)

func init() {
	tools.Register("calendar_list", Handle)
}

// Handle lists calendar events from all configured calendars in a date range.
func Handle(argsJSON string, ctx *tools.Context) string {
	// Parse arguments
	var args struct {
		Start string `json:"start"`
		End   string `json:"end"`
	}

	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error: failed to parse arguments: %v", err)
	}

	// Build comma-separated calendar list from config
	calendarList := strings.Join(ctx.Cfg.Calendar.Calendars, ",")
	if calendarList == "" {
		return "error: no calendars configured (add calendars to config.yaml)"
	}

	// Build bridge request
	req := calendar.Request{
		Command:  "list",
		Calendar: calendarList, // e.g., "Calendar,Work,Testing"
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

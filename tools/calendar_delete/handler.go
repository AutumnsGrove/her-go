package calendar_delete

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
	tools.Register("calendar_delete", Handle)
}

// Handle deletes a calendar event by ID.
func Handle(argsJSON string, ctx *tools.Context) string {
	// Parse arguments
	var args struct {
		EventID string `json:"event_id"`
	}

	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error: failed to parse arguments: %v", err)
	}

	// Validate event_id is present
	if args.EventID == "" {
		return "error: event_id is required"
	}

	// Build bridge request
	req := calendar.Request{
		Command:  "delete",
		Calendar: ctx.Cfg.Calendar.CalendarName,
		Args: map[string]any{
			"id": args.EventID,
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

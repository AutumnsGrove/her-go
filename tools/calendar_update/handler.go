package calendar_update

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
	tools.Register("calendar_update", Handle)
}

// Handle updates an existing calendar event by ID.
func Handle(argsJSON string, ctx *tools.Context) string {
	// Parse arguments (all fields except event_id are optional)
	var args struct {
		EventID  string `json:"event_id"`
		Title    string `json:"title,omitempty"`
		Start    string `json:"start,omitempty"`
		End      string `json:"end,omitempty"`
		Location string `json:"location,omitempty"`
		Notes    string `json:"notes,omitempty"`
	}

	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error: failed to parse arguments: %v", err)
	}

	// Validate event_id is present
	if args.EventID == "" {
		return "error: event_id is required"
	}

	// Build event update object (only include provided fields)
	eventUpdate := make(map[string]any)
	if args.Title != "" {
		eventUpdate["title"] = args.Title
	}
	if args.Start != "" {
		eventUpdate["start"] = args.Start
	}
	if args.End != "" {
		eventUpdate["end"] = args.End
	}
	if args.Location != "" {
		eventUpdate["location"] = args.Location
	}
	if args.Notes != "" {
		eventUpdate["notes"] = args.Notes
	}

	// Build bridge request
	req := calendar.Request{
		Command:  "update",
		Calendar: "*", // Search all configured calendars
		Args: map[string]any{
			"id":    args.EventID,
			"event": eventUpdate,
		},
	}

	// Initialize bridge
	logger := log.Default()
	bridge := calendar.NewCLIBridge(ctx.Cfg, logger)

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

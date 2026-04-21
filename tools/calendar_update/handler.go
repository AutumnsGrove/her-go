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
// SQLite is the source of truth — we update locally first, then sync to EventKit.
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

	// Step 1: Look up the event in SQLite to get the database ID.
	event, err := ctx.Store.GetCalendarEventByEventID(args.EventID)
	if err != nil {
		return fmt.Sprintf("error: failed to look up event: %v", err)
	}
	if event == nil {
		return fmt.Sprintf("error: event %s not found in local database", args.EventID)
	}

	// Step 2: Update the local SQLite row (sparse update).
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

	if len(eventUpdate) > 0 {
		if err := ctx.Store.UpdateCalendarEvent(event.ID, eventUpdate); err != nil {
			return fmt.Sprintf("error: failed to update event locally: %v", err)
		}
	}

	// Step 3: Sync the update to EventKit via the bridge.
	req := calendar.Request{
		Command:  "update",
		Calendar: "*", // Search all configured calendars
		Args: map[string]any{
			"id":    args.EventID,
			"event": eventUpdate,
		},
	}

	logger := log.Default()
	bridge := ctx.CalendarBridge
	if bridge == nil {
		bridge = calendar.NewCLIBridge(ctx.Cfg, logger)
	}

	callCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := bridge.Call(callCtx, req)
	if err != nil {
		// EventKit sync failed, but the local row is updated.
		log.Warn("EventKit sync failed for update", "event_id", args.EventID, "error", err)
		return fmt.Sprintf("warning: event updated locally but EventKit sync failed: %v", err)
	}

	// Format response as JSON for the agent
	resultJSON, err := json.Marshal(resp.Result)
	if err != nil {
		return fmt.Sprintf("error: failed to marshal result: %v", err)
	}

	return string(resultJSON)
}

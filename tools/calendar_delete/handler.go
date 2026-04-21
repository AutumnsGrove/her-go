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
// SQLite is the source of truth — we delete locally first, then sync to EventKit.
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

	// Step 1: Look up the event in SQLite to get the database ID.
	event, err := ctx.Store.GetCalendarEventByEventID(args.EventID)
	if err != nil {
		return fmt.Sprintf("error: failed to look up event: %v", err)
	}
	if event == nil {
		return fmt.Sprintf("error: event %s not found in local database", args.EventID)
	}

	// Step 2: Delete from the local SQLite database.
	if err := ctx.Store.DeleteCalendarEvent(event.ID); err != nil {
		return fmt.Sprintf("error: failed to delete event locally: %v", err)
	}

	// Step 3: Sync the deletion to EventKit via the bridge.
	req := calendar.Request{
		Command:  "delete",
		Calendar: "*", // Search all configured calendars
		Args: map[string]any{
			"id": args.EventID,
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
		// EventKit sync failed, but the local row is deleted.
		// This is acceptable — the deletion will stick locally, and the user
		// can manually clean up EventKit if needed.
		log.Warn("EventKit sync failed for delete", "event_id", args.EventID, "error", err)
		return fmt.Sprintf("warning: event deleted locally but EventKit sync failed: %v", err)
	}

	// Format response as JSON for the agent
	resultJSON, err := json.Marshal(resp.Result)
	if err != nil {
		return fmt.Sprintf("error: failed to marshal result: %v", err)
	}

	return string(resultJSON)
}

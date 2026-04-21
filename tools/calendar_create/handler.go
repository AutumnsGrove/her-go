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
// SQLite is the source of truth — we insert locally first, then sync to EventKit.
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

	// Step 1: Insert events into SQLite (source of truth).
	// Store the database IDs so we can update them with event_ids after syncing.
	dbIDs := make([]int64, len(args.Events))
	for i, event := range args.Events {
		title, _ := event["title"].(string)
		start, _ := event["start"].(string)
		end, _ := event["end"].(string)
		location, _ := event["location"].(string)
		notes, _ := event["notes"].(string)
		calendarName, _ := event["calendar"].(string)

		// Insert with empty event_id (will be filled after EventKit sync)
		dbID, err := ctx.Store.InsertCalendarEvent(title, start, end, location, notes, calendarName, "")
		if err != nil {
			log.Warn("failed to insert calendar event to DB", "error", err)
			return fmt.Sprintf("error: failed to save event locally: %v", err)
		}
		dbIDs[i] = dbID
	}

	// Step 2: Sync to EventKit via the bridge.
	req := calendar.Request{
		Command:  "create",
		Calendar: ctx.Cfg.Calendar.DefaultCalendar, // Swift uses this as fallback
		Args: map[string]any{
			"events": args.Events,
		},
	}

	logger := log.Default()
	bridge := ctx.CalendarBridge
	if bridge == nil {
		bridge = calendar.NewCLIBridge(ctx.Cfg, logger)
	}

	callCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := bridge.Call(callCtx, req)
	if err != nil {
		// EventKit sync failed, but events are saved locally with null event_id.
		// Return an error but don't delete the local rows — they can be manually
		// synced later or the user can retry.
		log.Warn("EventKit sync failed, events saved locally only", "error", err)
		return fmt.Sprintf("warning: events saved locally but EventKit sync failed: %v", err)
	}

	// Step 3: Update local rows with EventKit identifiers.
	// The bridge returns {"events": [{"id": "..."}, ...]} — extract the ID strings.
	if events, ok := resp.Result["events"].([]any); ok {
		for i, eventItem := range events {
			if i >= len(dbIDs) {
				break // safety: don't index out of bounds
			}
			// Each event is a map with an "id" field
			if eventMap, ok := eventItem.(map[string]any); ok {
				if eventID, ok := eventMap["id"].(string); ok {
					if err := ctx.Store.UpdateCalendarEventID(dbIDs[i], eventID); err != nil {
						log.Warn("failed to update event_id in DB", "db_id", dbIDs[i], "event_id", eventID, "error", err)
					}
				}
			}
		}
	}

	// Format response as JSON for the agent
	resultJSON, err := json.Marshal(resp.Result)
	if err != nil {
		return fmt.Sprintf("error: failed to marshal result: %v", err)
	}

	return string(resultJSON)
}

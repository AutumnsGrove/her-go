package calendar_list

import (
	"encoding/json"
	"fmt"

	"her/tools"
)

func init() {
	tools.Register("calendar_list", Handle)
}

// Handle lists calendar events from all configured calendars in a date range.
// SQLite is the source of truth — we query locally instead of shelling out to EventKit.
func Handle(argsJSON string, ctx *tools.Context) string {
	// Parse arguments
	var args struct {
		Start string `json:"start"`
		End   string `json:"end"`
	}

	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error: failed to parse arguments: %v", err)
	}

	// Query SQLite for events in the date range.
	// This is MUCH faster than shelling out to Swift + EventKit on every list.
	events, err := ctx.Store.ListCalendarEvents(args.Start, args.End)
	if err != nil {
		return fmt.Sprintf("error: failed to query events: %v", err)
	}

	// Format events into the same structure the bridge would return.
	// The agent expects: {"events": [{"id": "...", "title": "...", ...}, ...]}
	eventList := make([]map[string]any, len(events))
	for i, e := range events {
		eventList[i] = map[string]any{
			"id":       e.EventID,
			"title":    e.Title,
			"start":    e.Start.Format("2006-01-02T15:04:05"),
			"end":      e.End.Format("2006-01-02T15:04:05"),
			"calendar": e.Calendar,
		}
		// Only include optional fields if they're non-empty
		if e.Location != "" {
			eventList[i]["location"] = e.Location
		}
		if e.Notes != "" {
			eventList[i]["notes"] = e.Notes
		}
	}

	result := map[string]any{
		"events": eventList,
	}

	// Format response as JSON for the agent
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return fmt.Sprintf("error: failed to marshal result: %v", err)
	}

	return string(resultJSON)
}

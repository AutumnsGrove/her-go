package calendar_list

import (
	"encoding/json"
	"fmt"
	"math"

	"her/tools"
)

func init() {
	tools.Register("calendar_list", Handle)
}

// Handle lists calendar events from all configured calendars in a date range.
// Supports optional job filter and shifts_only flag for shift-specific queries.
// For shift events, includes job name, parsed shift metadata, and computed
// scheduled hours so the agent never has to do time math.
//
// SQLite is the source of truth — we query locally instead of shelling out to EventKit.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		Start      string `json:"start"`
		End        string `json:"end"`
		Job        string `json:"job,omitempty"`
		ShiftsOnly bool   `json:"shifts_only,omitempty"`
	}

	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error: failed to parse arguments: %v", err)
	}

	// Query SQLite for events in the date range.
	events, err := ctx.Store.ListCalendarEvents(args.Start, args.End, args.Job, args.ShiftsOnly)
	if err != nil {
		return fmt.Sprintf("error: failed to query events: %v", err)
	}

	// Format events into the response structure.
	// The agent expects: {"events": [...], "count": N}
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

		// For shift events, enrich the response with parsed shift metadata
		// and computed scheduled hours. This lets the agent answer questions
		// like "what shifts do I have" or "how long is my Friday shift"
		// without doing any math or parsing.
		if e.Job != "" {
			eventList[i]["job"] = e.Job

			// Compute scheduled hours from start/end. math.Round to 2 decimal
			// places avoids floating-point noise like 5.999999999.
			scheduledHours := e.End.Sub(e.Start).Hours()
			eventList[i]["scheduled_hours"] = math.Round(scheduledHours*100) / 100

			// Parse shift metadata from notes
			sn := tools.ParseShiftNotes(e.Notes)
			if sn.Position != "" {
				eventList[i]["position"] = sn.Position
			}
			if sn.Trainer != "" {
				eventList[i]["trainer"] = sn.Trainer
			}
			if sn.TimeChit != "" {
				eventList[i]["time_chit"] = sn.TimeChit
			}
			// Include freeform notes separately from shift metadata
			if sn.Freeform != "" {
				eventList[i]["notes"] = sn.Freeform
			}
		} else if e.Notes != "" {
			// Regular event — notes are just plain text
			eventList[i]["notes"] = e.Notes
		}
	}

	result := map[string]any{
		"events": eventList,
		"count":  len(eventList),
	}

	resultJSON, err := json.Marshal(result)
	if err != nil {
		return fmt.Sprintf("error: failed to marshal result: %v", err)
	}

	return string(resultJSON)
}

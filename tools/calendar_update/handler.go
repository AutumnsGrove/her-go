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
// For shift events, supports time_chit, position, trainer, and shift_notes
// params that are merged into the event's notes field as key: value pairs.
// SQLite is the source of truth — we update locally first, then sync to EventKit.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		EventID    string `json:"event_id"`
		Title      string `json:"title,omitempty"`
		Start      string `json:"start,omitempty"`
		End        string `json:"end,omitempty"`
		Location   string `json:"location,omitempty"`
		Notes      string `json:"notes,omitempty"`
		Job        string `json:"job,omitempty"`
		Position   string `json:"position,omitempty"`
		Trainer    string `json:"trainer,omitempty"`
		TimeChit   string `json:"time_chit,omitempty"`
		ShiftNotes string `json:"shift_notes,omitempty"`
	}

	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error: failed to parse arguments: %v", err)
	}

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

	// Step 2: Build the sparse update map for standard fields.
	// In Go, we build a map[string]any for the fields we want to update —
	// only non-empty values get included. This is the "sparse update" pattern.
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
	if args.Job != "" {
		// Normalize job name via config lookup
		if jobCfg := ctx.Cfg.Calendar.MatchJob(args.Job); jobCfg != nil {
			eventUpdate["job"] = jobCfg.Name
		} else {
			eventUpdate["job"] = args.Job
		}
	}

	// Step 3: Handle shift metadata in notes.
	// If any shift params are provided (position, trainer, time_chit, shift_notes),
	// we merge them into the existing notes using the key: value parser.
	// "notes" (plain) replaces freeform text; shift_notes also replaces freeform.
	hasShiftParams := args.Position != "" || args.Trainer != "" || args.TimeChit != "" || args.ShiftNotes != ""

	if hasShiftParams || args.Notes != "" {
		// Start with the current notes from the DB
		existingNotes := event.Notes

		if hasShiftParams {
			// Merge shift metadata into the notes. MergeShiftNotes parses
			// the existing key: value pairs, updates the provided fields,
			// and serializes back. Empty strings = leave unchanged.
			freeform := args.ShiftNotes
			if freeform == "" {
				freeform = args.Notes // "notes" param also sets freeform
			}
			existingNotes = tools.MergeShiftNotes(existingNotes, args.Position, args.Trainer, args.TimeChit, freeform)
		} else if args.Notes != "" {
			// No shift params, just plain notes — merge to preserve any
			// existing shift metadata while replacing freeform text.
			existingNotes = tools.MergeShiftNotes(existingNotes, "", "", "", args.Notes)
		}

		eventUpdate["notes"] = existingNotes
	}

	// Step 4: Apply the update to SQLite.
	if len(eventUpdate) > 0 {
		if err := ctx.Store.UpdateCalendarEvent(event.ID, eventUpdate); err != nil {
			return fmt.Sprintf("error: failed to update event locally: %v", err)
		}
	}

	// Step 5: Sync the update to EventKit via the bridge.
	// Build a bridge-friendly update map (only standard calendar fields —
	// the bridge doesn't know about shift metadata, it just sees notes).
	bridgeUpdate := make(map[string]any)
	for _, key := range []string{"title", "start", "end", "location", "notes"} {
		if v, ok := eventUpdate[key]; ok {
			bridgeUpdate[key] = v
		}
	}

	if len(bridgeUpdate) > 0 {
		req := calendar.Request{
			Command:  "update",
			Calendar: "*",
			Args: map[string]any{
				"id":    args.EventID,
				"event": bridgeUpdate,
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
			log.Warn("EventKit sync failed for update", "event_id", args.EventID, "error", err)
			return fmt.Sprintf("warning: event updated locally but EventKit sync failed: %v", err)
		}

		resultJSON, err := json.Marshal(resp.Result)
		if err != nil {
			return fmt.Sprintf("error: failed to marshal result: %v", err)
		}
		return string(resultJSON)
	}

	// If only shift metadata changed (no bridge-facing fields), return success
	return fmt.Sprintf(`{"event_id":"%s","updated":true}`, args.EventID)
}

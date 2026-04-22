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
// When a "job" param is present, the handler auto-fills location and position
// from config, serializes shift metadata into the notes field, and stores
// the job in the DB column for fast querying.
//
// SQLite is the source of truth — we insert locally first, then sync to EventKit.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		Events []map[string]any `json:"events"`
	}

	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error: failed to parse arguments: %v", err)
	}

	if len(args.Events) == 0 {
		return "error: no events provided"
	}

	// For each event, set default calendar if not specified
	for i := range args.Events {
		if _, hasCalendar := args.Events[i]["calendar"]; !hasCalendar {
			args.Events[i]["calendar"] = ctx.Cfg.Calendar.DefaultCalendar
		}
	}

	// Step 1: Process each event — apply shift defaults, build notes, insert to DB.
	dbIDs := make([]int64, len(args.Events))
	var warnings []string

	for i, event := range args.Events {
		title, _ := event["title"].(string)
		start, _ := event["start"].(string)
		end, _ := event["end"].(string)
		location, _ := event["location"].(string)
		notes, _ := event["notes"].(string)
		calendarName, _ := event["calendar"].(string)

		// Shift fields — all optional
		job, _ := event["job"].(string)
		position, _ := event["position"].(string)
		trainer, _ := event["trainer"].(string)

		// When job is provided, apply config defaults and build shift notes.
		// MatchJob does case-insensitive + alias lookup against the config's
		// job list — similar to how Python's dict.get() works with a fallback,
		// but here we return a pointer (nil = not found).
		if job != "" {
			jobCfg := ctx.Cfg.Calendar.MatchJob(job)
			if jobCfg != nil {
				// Normalize job name to the config's canonical form
				// (e.g., "panera bread" → "Panera")
				job = jobCfg.Name

				// Auto-fill location from config if not explicitly provided
				if location == "" && jobCfg.Address != "" {
					location = jobCfg.Address
					args.Events[i]["location"] = location
				}

				// Auto-fill position from config's default_role if not provided
				if position == "" && jobCfg.DefaultRole != "" {
					position = jobCfg.DefaultRole
				}
			}

			// Build shift metadata into the notes field. Any existing notes
			// text becomes freeform content below the key: value pairs.
			notes = tools.BuildShiftNotes(position, trainer, notes)
			args.Events[i]["notes"] = notes

			// Check for overlapping shifts — warn but don't block.
			// This queries active shift events that overlap with the new
			// shift's time range. Overlaps are common in real life (e.g.,
			// picking up a shift at one job while already scheduled at another).
			if ctx.Store != nil && start != "" && end != "" {
				overlaps, err := ctx.Store.ListShiftEvents(start, end, "")
				if err == nil && len(overlaps) > 0 {
					for _, o := range overlaps {
						warnings = append(warnings, fmt.Sprintf(
							"shift %d (%s %s–%s) overlaps with existing %s shift (id %d)",
							i+1, title, start, end, o.Job,
							o.ID,
						))
					}
				}
			}
		}

		// Insert with empty event_id (will be filled after EventKit sync)
		dbID, err := ctx.Store.InsertCalendarEvent(title, start, end, location, notes, calendarName, "", job)
		if err != nil {
			log.Warn("failed to insert calendar event to DB", "error", err)
			return fmt.Sprintf("error: failed to save event locally: %v", err)
		}
		dbIDs[i] = dbID
	}

	// Step 2: Sync to EventKit via the bridge.
	req := calendar.Request{
		Command:  "create",
		Calendar: ctx.Cfg.Calendar.DefaultCalendar,
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
		log.Warn("EventKit sync failed, events saved locally only", "error", err)
		return fmt.Sprintf("warning: events saved locally but EventKit sync failed: %v", err)
	}

	// Step 3: Update local rows with EventKit identifiers.
	if events, ok := resp.Result["events"].([]any); ok {
		for i, eventItem := range events {
			if i >= len(dbIDs) {
				break
			}
			if eventMap, ok := eventItem.(map[string]any); ok {
				if eventID, ok := eventMap["id"].(string); ok {
					if err := ctx.Store.UpdateCalendarEventID(dbIDs[i], eventID); err != nil {
						log.Warn("failed to update event_id in DB", "db_id", dbIDs[i], "event_id", eventID, "error", err)
					}
				}
			}
		}
	}

	// Build response — include warnings if any overlaps were detected
	result := resp.Result
	if result == nil {
		result = map[string]any{}
	}
	if len(warnings) > 0 {
		result["warnings"] = warnings
	}

	resultJSON, err := json.Marshal(result)
	if err != nil {
		return fmt.Sprintf("error: failed to marshal result: %v", err)
	}

	return string(resultJSON)
}

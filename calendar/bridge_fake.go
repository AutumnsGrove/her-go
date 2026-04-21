package calendar

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// FakeBridge is an in-memory calendar implementation for tests and sims.
// No Swift binary, no EventKit permissions — just a map of events.
// Supports all 5 bridge commands: list_calendars, list, create, update, delete.
type FakeBridge struct {
	events    map[string]*FakeEvent // keyed by event ID
	calendars []string               // available calendar names
	counter   int                    // for generating FAKE-001, FAKE-002...
	mu        sync.Mutex
}

// FakeEvent represents an in-memory calendar event.
// Matches the structure returned by the Swift bridge.
type FakeEvent struct {
	ID       string
	Title    string
	Start    time.Time
	End      time.Time
	Location string
	Notes    string
	Calendar string // which calendar this event belongs to
}

// NewFakeBridge creates a FakeBridge with the given calendar names.
// All calendars start empty — use Seed() or Call("create", ...) to populate.
func NewFakeBridge(calendars []string) *FakeBridge {
	return &FakeBridge{
		events:    make(map[string]*FakeEvent),
		calendars: calendars,
		counter:   0,
	}
}

// Call implements the Bridge interface for in-memory operations.
// Supports the same wire protocol as CLIBridge: Request -> Response.
func (f *FakeBridge) Call(ctx context.Context, req Request) (Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch req.Command {
	case "list_calendars":
		return f.listCalendars(req)
	case "list":
		return f.list(req)
	case "create":
		return f.create(req)
	case "update":
		return f.update(req)
	case "delete":
		return f.delete(req)
	default:
		return Response{
			OK:      false,
			Error:   "unknown_command",
			Message: fmt.Sprintf("unknown command: %s", req.Command),
		}, nil
	}
}

// Seed adds events to the fake bridge's in-memory store.
// Used by sims to populate pre-existing calendar state before the message loop.
func (f *FakeBridge) Seed(events []*FakeEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, event := range events {
		f.events[event.ID] = event
	}
}

// listCalendars returns all configured calendar names.
// Args: none
// Returns: {"calendars": ["Calendar", "Work", "Testing"]}
func (f *FakeBridge) listCalendars(req Request) (Response, error) {
	return Response{
		OK: true,
		Result: map[string]any{
			"calendars": f.calendars,
		},
	}, nil
}

// list returns events from matching calendars within a date range.
// Args: start (ISO8601), end (ISO8601)
// Calendar field: comma-separated list or "*" for all calendars
// Returns: {"events": [{id, title, start, end, location, notes, calendar}, ...]}
func (f *FakeBridge) list(req Request) (Response, error) {
	// Parse time range
	startStr, _ := req.Args["start"].(string)
	endStr, _ := req.Args["end"].(string)

	start, err := time.Parse(time.RFC3339, startStr)
	if err != nil {
		return Response{
			OK:      false,
			Error:   "invalid_start",
			Message: fmt.Sprintf("invalid start time: %v", err),
		}, nil
	}

	end, err := time.Parse(time.RFC3339, endStr)
	if err != nil {
		return Response{
			OK:      false,
			Error:   "invalid_end",
			Message: fmt.Sprintf("invalid end time: %v", err),
		}, nil
	}

	// Parse calendar filter (comma-separated or "*")
	calendarFilter := strings.Split(req.Calendar, ",")
	if req.Calendar == "*" {
		calendarFilter = f.calendars
	}

	// Filter events by calendar and time range
	var matchingEvents []map[string]any
	for _, event := range f.events {
		// Check calendar match
		if !contains(calendarFilter, event.Calendar) {
			continue
		}

		// Check time overlap (event overlaps if it starts before range end and ends after range start)
		if event.Start.Before(end) && event.End.After(start) {
			matchingEvents = append(matchingEvents, map[string]any{
				"id":       event.ID,
				"title":    event.Title,
				"start":    event.Start.Format(time.RFC3339),
				"end":      event.End.Format(time.RFC3339),
				"location": event.Location,
				"notes":    event.Notes,
				"calendar": event.Calendar,
			})
		}
	}

	return Response{
		OK: true,
		Result: map[string]any{
			"events": matchingEvents,
		},
	}, nil
}

// create adds one or more events to the calendar.
// Args: events: [{title, start, end, location?, notes?, calendar?}, ...]
// Calendar field: default calendar if not specified per-event
// Returns: {"event_ids": ["FAKE-001", "FAKE-002", ...]}
func (f *FakeBridge) create(req Request) (Response, error) {
	// Parse events array
	eventsRaw, ok := req.Args["events"].([]any)
	if !ok {
		return Response{
			OK:      false,
			Error:   "missing_events",
			Message: "events array is required",
		}, nil
	}

	var createdIDs []string

	for _, eventRaw := range eventsRaw {
		eventMap, ok := eventRaw.(map[string]any)
		if !ok {
			return Response{
				OK:      false,
				Error:   "invalid_event",
				Message: "each event must be an object",
			}, nil
		}

		// Extract fields
		title, _ := eventMap["title"].(string)
		startStr, _ := eventMap["start"].(string)
		endStr, _ := eventMap["end"].(string)
		location, _ := eventMap["location"].(string)
		notes, _ := eventMap["notes"].(string)

		// Calendar: per-event, fallback to request Calendar field
		calendar, _ := eventMap["calendar"].(string)
		if calendar == "" {
			calendar = req.Calendar
		}

		// Parse times
		start, err := time.Parse(time.RFC3339, startStr)
		if err != nil {
			return Response{
				OK:      false,
				Error:   "invalid_start",
				Message: fmt.Sprintf("invalid start time: %v", err),
			}, nil
		}

		end, err := time.Parse(time.RFC3339, endStr)
		if err != nil {
			return Response{
				OK:      false,
				Error:   "invalid_end",
				Message: fmt.Sprintf("invalid end time: %v", err),
			}, nil
		}

		// Generate deterministic ID
		f.counter++
		eventID := fmt.Sprintf("FAKE-%03d", f.counter)

		// Store event
		f.events[eventID] = &FakeEvent{
			ID:       eventID,
			Title:    title,
			Start:    start,
			End:      end,
			Location: location,
			Notes:    notes,
			Calendar: calendar,
		}

		createdIDs = append(createdIDs, eventID)
	}

	return Response{
		OK: true,
		Result: map[string]any{
			"event_ids": createdIDs,
		},
	}, nil
}

// update modifies an existing event by ID.
// Args: id (string), event: {title?, start?, end?, location?, notes?}
// Calendar field: "*" to search all calendars (or specific calendar name)
// Returns: {"event_id": "FAKE-001"}
func (f *FakeBridge) update(req Request) (Response, error) {
	// Parse event ID
	eventID, _ := req.Args["id"].(string)
	if eventID == "" {
		return Response{
			OK:      false,
			Error:   "missing_id",
			Message: "event id is required",
		}, nil
	}

	// Find event
	event, exists := f.events[eventID]
	if !exists {
		return Response{
			OK:      false,
			Error:   "event_not_found",
			Message: fmt.Sprintf("event %s not found", eventID),
		}, nil
	}

	// Parse update fields
	updateMap, ok := req.Args["event"].(map[string]any)
	if !ok {
		return Response{
			OK:      false,
			Error:   "missing_event",
			Message: "event update object is required",
		}, nil
	}

	// Apply updates (only provided fields)
	if title, ok := updateMap["title"].(string); ok {
		event.Title = title
	}
	if startStr, ok := updateMap["start"].(string); ok {
		if start, err := time.Parse(time.RFC3339, startStr); err == nil {
			event.Start = start
		}
	}
	if endStr, ok := updateMap["end"].(string); ok {
		if end, err := time.Parse(time.RFC3339, endStr); err == nil {
			event.End = end
		}
	}
	if location, ok := updateMap["location"].(string); ok {
		event.Location = location
	}
	if notes, ok := updateMap["notes"].(string); ok {
		event.Notes = notes
	}

	return Response{
		OK: true,
		Result: map[string]any{
			"event_id": eventID,
		},
	}, nil
}

// delete removes an event by ID.
// Args: id (string)
// Calendar field: "*" to search all calendars (or specific calendar name)
// Returns: {"deleted": true}
func (f *FakeBridge) delete(req Request) (Response, error) {
	// Parse event ID
	eventID, _ := req.Args["id"].(string)
	if eventID == "" {
		return Response{
			OK:      false,
			Error:   "missing_id",
			Message: "event id is required",
		}, nil
	}

	// Check if event exists
	if _, exists := f.events[eventID]; !exists {
		return Response{
			OK:      false,
			Error:   "event_not_found",
			Message: fmt.Sprintf("event %s not found", eventID),
		}, nil
	}

	// Delete event
	delete(f.events, eventID)

	return Response{
		OK: true,
		Result: map[string]any{
			"deleted": true,
		},
	}, nil
}

// contains checks if a string slice contains a value.
func contains(slice []string, value string) bool {
	for _, item := range slice {
		if item == value {
			return true
		}
	}
	return false
}

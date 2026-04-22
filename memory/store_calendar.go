package memory

import (
	"database/sql"
	"fmt"
	"time"
)

// CalendarEvent represents an event in the local SQLite mirror of the user's
// Apple Calendar. The event_id field is the EventKit identifier; it's nullable
// because events may fail to sync to EventKit but should still be tracked locally.
//
// This struct mirrors the calendar_events table schema. In Python you'd use
// a dataclass or attrs; in Go, structs with explicit fields are the standard.
type CalendarEvent struct {
	ID        int64
	EventID   string // EventKit identifier (may be empty if sync failed)
	Title     string
	Start     time.Time
	End       time.Time
	Location  string
	Notes     string
	Calendar  string // which calendar it belongs to (e.g., "Calendar", "Work")
	Job       string // nullable — empty for regular events, job name for shifts (e.g., "Panera")
	CreatedAt time.Time
	UpdatedAt time.Time
}

// InsertCalendarEvent inserts a new event into the local SQLite mirror.
// eventID is the EventKit identifier — pass an empty string if the event
// hasn't synced to EventKit yet. job is the shift job name — pass an empty
// string for regular (non-shift) events. Returns the database row ID.
//
// This is the first step in the calendar_create handler: write locally,
// then sync to EventKit, then update the row with the returned event_id.
func (s *Store) InsertCalendarEvent(title, start, end, location, notes, calendar, eventID, job string) (int64, error) {
	// In SQL, we use NULL to represent "no value yet" for optional fields.
	// In Go, we convert empty strings to nil (interface{}) so SQLite stores NULL.
	// This is a common pattern — like Python's None vs. "" distinction.
	var eventIDVal interface{} = eventID
	if eventID == "" {
		eventIDVal = nil
	}

	var locationVal interface{} = location
	if location == "" {
		locationVal = nil
	}

	var notesVal interface{} = notes
	if notes == "" {
		notesVal = nil
	}

	var jobVal interface{} = job
	if job == "" {
		jobVal = nil
	}

	result, err := s.db.Exec(
		`INSERT INTO calendar_events (event_id, title, start, end, location, notes, calendar, job)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		eventIDVal, title, start, end, locationVal, notesVal, calendar, jobVal,
	)
	if err != nil {
		return 0, fmt.Errorf("inserting calendar event: %w", err)
	}

	// LastInsertId returns the auto-incremented ID that was just assigned.
	// This is like cursor.lastrowid in Python's sqlite3 module.
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting calendar event ID: %w", err)
	}

	return id, nil
}

// UpdateCalendarEvent updates an existing calendar event by database ID.
// The updates map contains field names as keys (e.g., "title", "start").
// Only the provided fields are updated — this is a sparse update.
//
// Go doesn't have Python's **kwargs, so we use a map[string]any to represent
// optional field updates. The handler builds this map from the JSON args.
func (s *Store) UpdateCalendarEvent(id int64, updates map[string]any) error {
	if len(updates) == 0 {
		return nil // no-op
	}

	// Build a dynamic SQL UPDATE statement with only the provided fields.
	// This is a common pattern for sparse updates — build the SET clause
	// from the map keys, then pass the values in the same order.
	query := "UPDATE calendar_events SET "
	args := []interface{}{}

	first := true
	for field, value := range updates {
		if !first {
			query += ", "
		}
		first = false

		// Map JSON field names to column names
		switch field {
		case "title":
			query += "title = ?"
		case "start":
			query += "start = ?"
		case "end":
			query += "end = ?"
		case "location":
			query += "location = ?"
		case "notes":
			query += "notes = ?"
		case "event_id":
			query += "event_id = ?"
		case "job":
			query += "job = ?"
		default:
			// Skip unknown fields (defensive — shouldn't happen in practice)
			continue
		}

		// Convert empty strings to NULL for optional fields
		if (field == "location" || field == "notes" || field == "job") && value == "" {
			args = append(args, nil)
		} else {
			args = append(args, value)
		}
	}

	// Set updated_at timestamp (ISO 8601 format to match SQLite's DATETIME)
	query += ", updated_at = ? WHERE id = ?"
	args = append(args, time.Now().Format("2006-01-02 15:04:05"), id)

	_, err := s.db.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("updating calendar event %d: %w", id, err)
	}

	return nil
}

// UpdateCalendarEventID sets the event_id for a calendar event after it's
// been synced to EventKit. This is called by calendar_create after the
// bridge returns the EventKit identifier.
func (s *Store) UpdateCalendarEventID(id int64, eventID string) error {
	_, err := s.db.Exec(
		`UPDATE calendar_events SET event_id = ?, updated_at = ? WHERE id = ?`,
		eventID, time.Now().Format("2006-01-02 15:04:05"), id,
	)
	if err != nil {
		return fmt.Errorf("updating event_id for calendar event %d: %w", id, err)
	}
	return nil
}

// DeleteCalendarEvent soft-deletes a calendar event by setting active = 0.
// This follows the memory system pattern (DeactivateMemory) — the event stays
// in the database for audit trail but won't appear in queries. This is called
// by calendar_delete before calling the bridge.
func (s *Store) DeleteCalendarEvent(id int64) error {
	_, err := s.db.Exec(
		`UPDATE calendar_events SET active = 0, updated_at = ? WHERE id = ?`,
		time.Now().Format("2006-01-02 15:04:05"), id,
	)
	if err != nil {
		return fmt.Errorf("deactivating calendar event %d: %w", id, err)
	}
	return nil
}

// ListCalendarEvents returns active events in the given date range.
// start and end should be ISO 8601 date strings (YYYY-MM-DD or YYYY-MM-DDTHH:MM:SS).
// job is an optional filter — if non-empty, only returns events with that job.
// If shiftsOnly is true, only returns events that have a job set (any job).
//
// This is the primary read path for calendar_list — querying SQLite instead of
// shelling out to EventKit on every list operation.
//
// The query uses string comparison on TEXT columns, which works correctly for
// ISO 8601 dates because they sort lexicographically (2026-04-21 < 2026-04-22).
func (s *Store) ListCalendarEvents(start, end, job string, shiftsOnly bool) ([]CalendarEvent, error) {
	// Build the query dynamically based on optional filters. In Go we build
	// the WHERE clause and args slice together — similar to how you'd use
	// a query builder in Python (like SQLAlchemy's filter chain).
	query := `SELECT id, COALESCE(event_id, ''), title, start, end,
	                 COALESCE(location, ''), COALESCE(notes, ''), calendar,
	                 COALESCE(job, ''), created_at, COALESCE(updated_at, '')
	          FROM calendar_events
	          WHERE active = 1 AND start >= ? AND end <= ?`
	args := []interface{}{start, end}

	if job != "" {
		query += " AND job = ?"
		args = append(args, job)
	} else if shiftsOnly {
		query += " AND job IS NOT NULL"
	}

	query += " ORDER BY start ASC"

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying calendar events: %w", err)
	}
	// defer is Go's cleanup mechanism — like Python's "with" statement or
	// finally block. This ensures rows.Close() runs when we return, even
	// if there's an error during scanning.
	defer rows.Close()

	var events []CalendarEvent
	for rows.Next() {
		var e CalendarEvent
		var startStr, endStr, createdAtStr, updatedAtStr string

		// Scan reads the current row into Go variables. The number and types
		// must match the SELECT columns exactly. COALESCE() in the query
		// converts NULL to empty string so we can scan into string types.
		if err := rows.Scan(
			&e.ID, &e.EventID, &e.Title, &startStr, &endStr,
			&e.Location, &e.Notes, &e.Calendar, &e.Job,
			&createdAtStr, &updatedAtStr,
		); err != nil {
			return nil, fmt.Errorf("scanning calendar event row: %w", err)
		}

		// Parse ISO 8601 timestamps into time.Time. Go's time package uses
		// a reference timestamp (2006-01-02 15:04:05) as the format string —
		// this is Go's quirky alternative to strftime format codes.
		e.Start, _ = time.Parse("2006-01-02 15:04:05", startStr)
		e.End, _ = time.Parse("2006-01-02 15:04:05", endStr)
		e.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAtStr)
		if updatedAtStr != "" {
			e.UpdatedAt, _ = time.Parse("2006-01-02 15:04:05", updatedAtStr)
		}

		events = append(events, e)
	}

	return events, nil
}

// GetCalendarEventByEventID looks up a calendar event by its EventKit identifier.
// Returns nil and no error if the event doesn't exist. Used by update/delete
// handlers to find the database ID from the event_id the agent provides.
func (s *Store) GetCalendarEventByEventID(eventID string) (*CalendarEvent, error) {
	var e CalendarEvent
	var startStr, endStr, createdAtStr string
	var locationVal, notesVal, jobVal, updatedAtVal sql.NullString

	err := s.db.QueryRow(
		`SELECT id, event_id, title, start, end, location, notes, calendar, job, created_at, updated_at
		 FROM calendar_events WHERE event_id = ? AND active = 1`,
		eventID,
	).Scan(
		&e.ID, &e.EventID, &e.Title, &startStr, &endStr,
		&locationVal, &notesVal, &e.Calendar, &jobVal, &createdAtStr, &updatedAtVal,
	)

	// sql.ErrNoRows means the query returned zero rows — this is not an error
	// in our case, we just return nil to indicate "not found". This is like
	// catching IndexError in Python when looking up a value.
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting calendar event %s: %w", eventID, err)
	}

	// Handle NULL values — sql.NullString has a Valid field that tells us
	// if the column was NULL or had a real string value.
	if locationVal.Valid {
		e.Location = locationVal.String
	}
	if notesVal.Valid {
		e.Notes = notesVal.String
	}
	if jobVal.Valid {
		e.Job = jobVal.String
	}

	// Parse timestamps
	e.Start, _ = time.Parse("2006-01-02 15:04:05", startStr)
	e.End, _ = time.Parse("2006-01-02 15:04:05", endStr)
	e.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAtStr)
	if updatedAtVal.Valid {
		e.UpdatedAt, _ = time.Parse("2006-01-02 15:04:05", updatedAtVal.String)
	}

	return &e, nil
}

// ListShiftEvents returns active calendar events that have a job set,
// filtered by date range and optionally by job name. This is the primary
// query path for the shift_hours tool — it only returns shift events
// (job IS NOT NULL), never regular calendar events.
//
// This is essentially ListCalendarEvents with shiftsOnly hardcoded to true,
// extracted as a separate method for clarity. The shift_hours handler calls
// this directly instead of passing flags through the general-purpose method.
func (s *Store) ListShiftEvents(start, end, job string) ([]CalendarEvent, error) {
	return s.ListCalendarEvents(start, end, job, true)
}

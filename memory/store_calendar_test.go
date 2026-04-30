package memory

import (
	"path/filepath"
	"testing"
)

// newCalendarTestStore creates a fresh in-memory Store for calendar tests.
// Same pattern as newMoodTestStore — temp directory auto-cleaned on test end.
func newCalendarTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "calendar_test.db")
	store, err := NewStore(dbPath, 0)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestInsertAndListCalendarEvents is the basic round-trip test: insert some
// events (with and without jobs), then list them back and verify the data
// comes out correct. This covers the happy path for the calendar_create →
// calendar_list flow.
func TestInsertAndListCalendarEvents(t *testing.T) {
	store := newCalendarTestStore(t)

	// Insert a regular event (no job)
	id1, err := store.InsertCalendarEvent(
		"Dentist", "2026-04-25 09:00:00", "2026-04-25 10:00:00",
		"123 Main St", "Cleaning", "Calendar", "", "",
	)
	if err != nil {
		t.Fatalf("insert regular event: %v", err)
	}
	if id1 == 0 {
		t.Fatal("expected non-zero ID for regular event")
	}

	// Insert a shift event (with job)
	id2, err := store.InsertCalendarEvent(
		"Panera", "2026-04-25 11:00:00", "2026-04-25 17:00:00",
		"3625 Spring Hill Pkwy", "position: Grill Cook", "Work", "", "Panera",
	)
	if err != nil {
		t.Fatalf("insert shift event: %v", err)
	}
	if id2 <= id1 {
		t.Fatalf("expected id2 > id1, got %d <= %d", id2, id1)
	}

	// List all events in the range
	events, err := store.ListCalendarEvents("2026-04-25 00:00:00", "2026-04-25 23:59:59", "", false)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}

	// First event is the dentist (earlier start time)
	if events[0].Title != "Dentist" {
		t.Errorf("events[0].Title = %q, want %q", events[0].Title, "Dentist")
	}
	if events[0].Job != "" {
		t.Errorf("events[0].Job = %q, want empty (regular event)", events[0].Job)
	}

	// Second event is the shift
	if events[1].Title != "Panera" {
		t.Errorf("events[1].Title = %q, want %q", events[1].Title, "Panera")
	}
	if events[1].Job != "Panera" {
		t.Errorf("events[1].Job = %q, want %q", events[1].Job, "Panera")
	}
	if events[1].Notes != "position: Grill Cook" {
		t.Errorf("events[1].Notes = %q, want %q", events[1].Notes, "position: Grill Cook")
	}
}

// TestListCalendarEvents_JobFilter verifies that the job parameter filters
// results to only shift events for that job.
func TestListCalendarEvents_JobFilter(t *testing.T) {
	store := newCalendarTestStore(t)

	// Insert events for different jobs + a regular event
	store.InsertCalendarEvent("Panera", "2026-04-25 09:00:00", "2026-04-25 15:00:00", "", "", "Work", "", "Panera")
	store.InsertCalendarEvent("Cava", "2026-04-25 16:00:00", "2026-04-25 22:00:00", "", "", "Work", "", "Cava")
	store.InsertCalendarEvent("Dentist", "2026-04-25 08:00:00", "2026-04-25 09:00:00", "", "", "Calendar", "", "")

	// Filter by Panera
	events, err := store.ListCalendarEvents("2026-04-25 00:00:00", "2026-04-25 23:59:59", "Panera", false)
	if err != nil {
		t.Fatalf("list with job filter: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 Panera event, got %d", len(events))
	}
	if events[0].Job != "Panera" {
		t.Errorf("filtered event job = %q, want %q", events[0].Job, "Panera")
	}
}

// TestListCalendarEvents_ShiftsOnly verifies that shiftsOnly=true excludes
// regular calendar events and returns only events with a job set.
func TestListCalendarEvents_ShiftsOnly(t *testing.T) {
	store := newCalendarTestStore(t)

	store.InsertCalendarEvent("Panera", "2026-04-25 09:00:00", "2026-04-25 15:00:00", "", "", "Work", "", "Panera")
	store.InsertCalendarEvent("Cava", "2026-04-25 16:00:00", "2026-04-25 22:00:00", "", "", "Work", "", "Cava")
	store.InsertCalendarEvent("Dentist", "2026-04-25 08:00:00", "2026-04-25 09:00:00", "", "", "Calendar", "", "")

	events, err := store.ListCalendarEvents("2026-04-25 00:00:00", "2026-04-25 23:59:59", "", true)
	if err != nil {
		t.Fatalf("list shifts only: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 shift events, got %d", len(events))
	}
	for _, e := range events {
		if e.Job == "" {
			t.Errorf("shiftsOnly returned event without job: %q", e.Title)
		}
	}
}

// TestListShiftEvents is a quick smoke test for the convenience wrapper.
// It delegates to ListCalendarEvents with shiftsOnly=true.
func TestListShiftEvents(t *testing.T) {
	store := newCalendarTestStore(t)

	store.InsertCalendarEvent("Panera", "2026-04-25 09:00:00", "2026-04-25 15:00:00", "", "", "Work", "", "Panera")
	store.InsertCalendarEvent("Dentist", "2026-04-25 08:00:00", "2026-04-25 09:00:00", "", "", "Calendar", "", "")

	events, err := store.ListShiftEvents("2026-04-25 00:00:00", "2026-04-25 23:59:59", "")
	if err != nil {
		t.Fatalf("ListShiftEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 shift, got %d", len(events))
	}
	if events[0].Job != "Panera" {
		t.Errorf("shift job = %q, want %q", events[0].Job, "Panera")
	}
}

// TestUpdateCalendarEvent_Job verifies that the job field can be set and
// updated via the sparse update path.
func TestUpdateCalendarEvent_Job(t *testing.T) {
	store := newCalendarTestStore(t)

	// Insert a regular event
	id, err := store.InsertCalendarEvent(
		"Work", "2026-04-25 09:00:00", "2026-04-25 15:00:00",
		"", "", "Work", "EVT-001", "",
	)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Update it to be a shift by adding a job
	err = store.UpdateCalendarEvent(id, map[string]any{"job": "Panera"})
	if err != nil {
		t.Fatalf("update job: %v", err)
	}

	// Verify via GetCalendarEventByEventID
	event, err := store.GetCalendarEventByEventID("EVT-001")
	if err != nil {
		t.Fatalf("get event: %v", err)
	}
	if event.Job != "Panera" {
		t.Errorf("event.Job = %q, want %q", event.Job, "Panera")
	}
}

// TestGetCalendarEventByEventID_IncludesJob verifies that the job field
// round-trips through insert → get correctly.
func TestGetCalendarEventByEventID_IncludesJob(t *testing.T) {
	store := newCalendarTestStore(t)

	store.InsertCalendarEvent(
		"Cava", "2026-04-25 16:00:00", "2026-04-25 22:00:00",
		"855 Peachtree", "position: Grill Cook", "Work", "EVT-002", "Cava",
	)

	event, err := store.GetCalendarEventByEventID("EVT-002")
	if err != nil {
		t.Fatalf("get event: %v", err)
	}
	if event == nil {
		t.Fatal("event not found")
	}
	if event.Job != "Cava" {
		t.Errorf("event.Job = %q, want %q", event.Job, "Cava")
	}
	if event.Location != "855 Peachtree" {
		t.Errorf("event.Location = %q, want %q", event.Location, "855 Peachtree")
	}
}

// TestDeleteCalendarEvent_ExcludesFromList verifies that soft-deleted events
// don't appear in list results (including shift queries).
func TestDeleteCalendarEvent_ExcludesFromList(t *testing.T) {
	store := newCalendarTestStore(t)

	id, _ := store.InsertCalendarEvent(
		"Panera", "2026-04-25 09:00:00", "2026-04-25 15:00:00",
		"", "", "Work", "", "Panera",
	)

	// Delete it
	if err := store.DeleteCalendarEvent(id); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Should not appear in list
	events, err := store.ListCalendarEvents("2026-04-25 00:00:00", "2026-04-25 23:59:59", "", false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events after delete, got %d", len(events))
	}

	// Should not appear in shift query either
	shifts, err := store.ListShiftEvents("2026-04-25 00:00:00", "2026-04-25 23:59:59", "")
	if err != nil {
		t.Fatalf("list shifts: %v", err)
	}
	if len(shifts) != 0 {
		t.Errorf("expected 0 shifts after delete, got %d", len(shifts))
	}
}

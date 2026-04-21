package calendar

import (
	"context"
	"testing"
	"time"
)

func TestFakeBridge_ListCalendars(t *testing.T) {
	bridge := NewFakeBridge([]string{"Calendar", "Work", "Testing"})

	req := Request{
		Command: "list_calendars",
		Args:    map[string]any{},
	}

	resp, err := bridge.Call(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !resp.OK {
		t.Fatalf("expected OK=true, got: %v", resp.Message)
	}

	calendars, ok := resp.Result["calendars"].([]string)
	if !ok {
		t.Fatalf("expected calendars array, got: %T", resp.Result["calendars"])
	}

	if len(calendars) != 3 {
		t.Errorf("expected 3 calendars, got %d", len(calendars))
	}
}

func TestFakeBridge_Create(t *testing.T) {
	bridge := NewFakeBridge([]string{"Calendar", "Work"})

	req := Request{
		Command:  "create",
		Calendar: "Work",
		Args: map[string]any{
			"events": []any{
				map[string]any{
					"title":    "Team Meeting",
					"start":    "2026-04-21T10:00:00-04:00",
					"end":      "2026-04-21T11:00:00-04:00",
					"location": "Office",
					"notes":    "Weekly sync",
				},
				map[string]any{
					"title":    "Lunch",
					"start":    "2026-04-21T12:00:00-04:00",
					"end":      "2026-04-21T13:00:00-04:00",
					"calendar": "Calendar", // Override default
				},
			},
		},
	}

	resp, err := bridge.Call(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !resp.OK {
		t.Fatalf("expected OK=true, got: %v", resp.Message)
	}

	eventIDs, ok := resp.Result["event_ids"].([]string)
	if !ok {
		t.Fatalf("expected event_ids array, got: %T", resp.Result["event_ids"])
	}

	if len(eventIDs) != 2 {
		t.Errorf("expected 2 event IDs, got %d", len(eventIDs))
	}

	// Verify deterministic IDs
	if eventIDs[0] != "FAKE-001" {
		t.Errorf("expected FAKE-001, got %s", eventIDs[0])
	}
	if eventIDs[1] != "FAKE-002" {
		t.Errorf("expected FAKE-002, got %s", eventIDs[1])
	}

	// Verify events stored correctly
	bridge.mu.Lock()
	defer bridge.mu.Unlock()

	event1 := bridge.events["FAKE-001"]
	if event1.Title != "Team Meeting" {
		t.Errorf("expected title 'Team Meeting', got '%s'", event1.Title)
	}
	if event1.Calendar != "Work" {
		t.Errorf("expected calendar 'Work', got '%s'", event1.Calendar)
	}

	event2 := bridge.events["FAKE-002"]
	if event2.Calendar != "Calendar" {
		t.Errorf("expected calendar 'Calendar' (per-event override), got '%s'", event2.Calendar)
	}
}

func TestFakeBridge_List(t *testing.T) {
	bridge := NewFakeBridge([]string{"Calendar", "Work"})

	// Seed events
	bridge.Seed([]*FakeEvent{
		{
			ID:       "FAKE-001",
			Title:    "Morning Meeting",
			Start:    mustParse("2026-04-21T09:00:00-04:00"),
			End:      mustParse("2026-04-21T10:00:00-04:00"),
			Calendar: "Work",
		},
		{
			ID:       "FAKE-002",
			Title:    "Dentist",
			Start:    mustParse("2026-04-22T14:00:00-04:00"),
			End:      mustParse("2026-04-22T15:00:00-04:00"),
			Calendar: "Calendar",
		},
		{
			ID:       "FAKE-003",
			Title:    "After Hours",
			Start:    mustParse("2026-04-23T18:00:00-04:00"),
			End:      mustParse("2026-04-23T19:00:00-04:00"),
			Calendar: "Work",
		},
	})

	tests := []struct {
		name           string
		calendar       string
		start          string
		end            string
		expectedCount  int
		expectedTitles []string
	}{
		{
			name:           "list all calendars in range",
			calendar:       "*",
			start:          "2026-04-21T00:00:00-04:00",
			end:            "2026-04-23T00:00:00-04:00",
			expectedCount:  2,
			expectedTitles: []string{"Morning Meeting", "Dentist"},
		},
		{
			name:           "list single calendar",
			calendar:       "Work",
			start:          "2026-04-21T00:00:00-04:00",
			end:            "2026-04-24T00:00:00-04:00",
			expectedCount:  2,
			expectedTitles: []string{"Morning Meeting", "After Hours"},
		},
		{
			name:           "list no events outside range",
			calendar:       "*",
			start:          "2026-04-25T00:00:00-04:00",
			end:            "2026-04-26T00:00:00-04:00",
			expectedCount:  0,
			expectedTitles: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := Request{
				Command:  "list",
				Calendar: tt.calendar,
				Args: map[string]any{
					"start": tt.start,
					"end":   tt.end,
				},
			}

			resp, err := bridge.Call(context.Background(), req)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if !resp.OK {
				t.Fatalf("expected OK=true, got: %v", resp.Message)
			}

			events, ok := resp.Result["events"].([]map[string]any)
			if !ok {
				t.Fatalf("expected events array, got: %T", resp.Result["events"])
			}

			if len(events) != tt.expectedCount {
				t.Errorf("expected %d events, got %d", tt.expectedCount, len(events))
			}

			// Check titles (order-independent — maps iterate randomly in Go)
			actualTitles := make(map[string]bool)
			for _, event := range events {
				title := event["title"].(string)
				actualTitles[title] = true
			}

			for _, expectedTitle := range tt.expectedTitles {
				if !actualTitles[expectedTitle] {
					t.Errorf("expected title '%s' not found in results", expectedTitle)
				}
			}
		})
	}
}

func TestFakeBridge_Update(t *testing.T) {
	bridge := NewFakeBridge([]string{"Calendar"})

	// Seed an event
	bridge.Seed([]*FakeEvent{
		{
			ID:       "FAKE-001",
			Title:    "Old Title",
			Start:    mustParse("2026-04-21T10:00:00-04:00"),
			End:      mustParse("2026-04-21T11:00:00-04:00"),
			Location: "Old Location",
			Calendar: "Calendar",
		},
	})

	req := Request{
		Command:  "update",
		Calendar: "*",
		Args: map[string]any{
			"id": "FAKE-001",
			"event": map[string]any{
				"title":    "New Title",
				"location": "New Location",
			},
		},
	}

	resp, err := bridge.Call(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !resp.OK {
		t.Fatalf("expected OK=true, got: %v", resp.Message)
	}

	eventID, ok := resp.Result["event_id"].(string)
	if !ok || eventID != "FAKE-001" {
		t.Fatalf("expected event_id FAKE-001, got: %v", resp.Result["event_id"])
	}

	// Verify update applied
	bridge.mu.Lock()
	defer bridge.mu.Unlock()

	event := bridge.events["FAKE-001"]
	if event.Title != "New Title" {
		t.Errorf("expected title 'New Title', got '%s'", event.Title)
	}
	if event.Location != "New Location" {
		t.Errorf("expected location 'New Location', got '%s'", event.Location)
	}
	// Start should be unchanged
	expectedStart := mustParse("2026-04-21T10:00:00-04:00")
	if !event.Start.Equal(expectedStart) {
		t.Errorf("expected start unchanged, got %v", event.Start)
	}
}

func TestFakeBridge_Update_NotFound(t *testing.T) {
	bridge := NewFakeBridge([]string{"Calendar"})

	req := Request{
		Command:  "update",
		Calendar: "*",
		Args: map[string]any{
			"id": "NONEXISTENT",
			"event": map[string]any{
				"title": "New Title",
			},
		},
	}

	resp, err := bridge.Call(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.OK {
		t.Fatal("expected OK=false for nonexistent event")
	}

	if resp.Error != "event_not_found" {
		t.Errorf("expected error 'event_not_found', got '%s'", resp.Error)
	}
}

func TestFakeBridge_Delete(t *testing.T) {
	bridge := NewFakeBridge([]string{"Calendar"})

	// Seed an event
	bridge.Seed([]*FakeEvent{
		{
			ID:       "FAKE-001",
			Title:    "To Delete",
			Start:    mustParse("2026-04-21T10:00:00-04:00"),
			End:      mustParse("2026-04-21T11:00:00-04:00"),
			Calendar: "Calendar",
		},
	})

	// Verify event exists
	bridge.mu.Lock()
	if _, exists := bridge.events["FAKE-001"]; !exists {
		t.Fatal("event should exist before delete")
	}
	bridge.mu.Unlock()

	req := Request{
		Command:  "delete",
		Calendar: "*",
		Args: map[string]any{
			"id": "FAKE-001",
		},
	}

	resp, err := bridge.Call(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !resp.OK {
		t.Fatalf("expected OK=true, got: %v", resp.Message)
	}

	deleted, ok := resp.Result["deleted"].(bool)
	if !ok || !deleted {
		t.Fatalf("expected deleted=true, got: %v", resp.Result["deleted"])
	}

	// Verify event removed
	bridge.mu.Lock()
	defer bridge.mu.Unlock()

	if _, exists := bridge.events["FAKE-001"]; exists {
		t.Error("event should not exist after delete")
	}
}

func TestFakeBridge_Delete_NotFound(t *testing.T) {
	bridge := NewFakeBridge([]string{"Calendar"})

	req := Request{
		Command:  "delete",
		Calendar: "*",
		Args: map[string]any{
			"id": "NONEXISTENT",
		},
	}

	resp, err := bridge.Call(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.OK {
		t.Fatal("expected OK=false for nonexistent event")
	}

	if resp.Error != "event_not_found" {
		t.Errorf("expected error 'event_not_found', got '%s'", resp.Error)
	}
}

func TestFakeBridge_UnknownCommand(t *testing.T) {
	bridge := NewFakeBridge([]string{"Calendar"})

	req := Request{
		Command: "invalid",
		Args:    map[string]any{},
	}

	resp, err := bridge.Call(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.OK {
		t.Fatal("expected OK=false for unknown command")
	}

	if resp.Error != "unknown_command" {
		t.Errorf("expected error 'unknown_command', got '%s'", resp.Error)
	}
}

// Helper: parse RFC3339 time or panic
func mustParse(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

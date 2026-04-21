package calendar

import (
	"context"
	"testing"

	"her/config"
	"github.com/charmbracelet/log"
)

// TestBridgeMissingBinary verifies that calling the bridge with a missing
// binary returns a clear error (fail-soft pattern).
func TestBridgeMissingBinary(t *testing.T) {
	cfg := &config.Config{
		Calendar: config.CalendarConfig{
			BridgePath:      "/nonexistent/path/to/binary",
			Calendars:       []string{"Test"},
			DefaultCalendar: "Test",
		},
	}

	logger := log.New(nil)
	logger.SetLevel(log.FatalLevel) // Silence logs during test

	bridge := NewCLIBridge(cfg, logger)

	req := Request{
		Command:  "list",
		Calendar: "Test",
		Args: map[string]any{
			"start": "2026-04-20T00:00:00-04:00",
			"end":   "2026-04-21T00:00:00-04:00",
		},
	}

	_, err := bridge.Call(context.Background(), req)
	if err == nil {
		t.Fatal("Expected error when bridge binary missing, got nil")
	}

	// Error message should mention the missing binary
	expected := "calendar bridge not found"
	if err.Error()[:len(expected)] != expected {
		t.Errorf("Expected error to start with %q, got %q", expected, err.Error())
	}
}

// Note: Full integration tests (calling the real Swift binary) should be run
// manually or in CI with proper permissions. Unit tests with a fake binary
// would require creating a shell script that echoes canned JSON, which we'll
// skip for now since the manual testing already validated the full flow.

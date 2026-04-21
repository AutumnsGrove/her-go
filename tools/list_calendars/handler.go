package list_calendars

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
	tools.Register("list_calendars", Handle)
}

// Handle lists all available calendars that events can be added to.
// No parameters required — returns a list of calendar names.
func Handle(argsJSON string, ctx *tools.Context) string {
	// Build bridge request (empty args for list_calendars)
	req := calendar.Request{
		Command:  "list_calendars",
		Calendar: "", // Not used for this command
		Args:     map[string]any{},
	}

	// Initialize bridge
	logger := log.Default()
	bridge := calendar.NewBridge(ctx.Cfg, logger)

	// Call with timeout
	callCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := bridge.Call(callCtx, req)
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}

	// Format response as JSON for the agent
	resultJSON, err := json.Marshal(resp.Result)
	if err != nil {
		return fmt.Sprintf("error: failed to marshal result: %v", err)
	}

	return string(resultJSON)
}

// Package summary implements the summary tool — records the worker
// agent's findings for return to the dispatching agent. Called before
// done to capture the result of the worker's work.
package summary

import (
	"encoding/json"

	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/summary")

func init() {
	tools.Register("summary", Handle)
}

// Handle stores the summary text on the tool context so RunWorker
// can extract it after the loop finishes.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "error: " + err.Error()
	}
	if args.Text == "" {
		return "error: text is required"
	}

	ctx.WorkerSummary = args.Text
	log.Infof("  summary: %d chars", len(args.Text))
	return "summary recorded"
}

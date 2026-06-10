// Package read_file implements the read_file tool — reads a file from the
// reports directory. Available to both the worker agent (for referencing
// previous reports while writing) and the driver agent (for answering
// questions about past reports).
package read_file

import (
	"encoding/json"
	"fmt"
	"os"

	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/read_file")

func init() {
	tools.Register("read_file", Handle)
}

// Handle reads a file from the reports directory and returns its content.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "error: " + err.Error()
	}
	if args.Path == "" {
		return "error: path is required"
	}

	absPath, err := tools.ValidateReportPath(ctx.ReportsDir, args.Path)
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}

	content, err := os.ReadFile(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Sprintf("error: file not found: %s", args.Path)
		}
		return fmt.Sprintf("error reading file: %v", err)
	}

	log.Infof("  read_file: %s (%d bytes)", args.Path, len(content))
	return string(content)
}

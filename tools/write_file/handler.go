// Package write_file implements the write_file tool — creates or overwrites
// a file in the reports directory. Used by the worker agent to produce
// report artifacts (markdown files, research notes, briefings).
//
// Paths are relative to the configured ReportsDir. Path traversal attempts
// (../../etc/passwd) are rejected by the shared validation in tools.ValidateReportPath.
package write_file

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/write_file")

func init() {
	tools.Register("write_file", Handle)
}

// Handle writes content to a file inside the reports directory.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "error: " + err.Error()
	}
	if args.Path == "" {
		return "error: path is required"
	}
	if args.Content == "" {
		return "error: content is required"
	}

	absPath, err := tools.ValidateReportPath(ctx.ReportsDir, args.Path)
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}

	// Create parent directories if they don't exist.
	if err := os.MkdirAll(filepath.Dir(absPath), 0755); err != nil {
		return fmt.Sprintf("error creating directories: %v", err)
	}

	if err := os.WriteFile(absPath, []byte(args.Content), 0644); err != nil {
		return fmt.Sprintf("error writing file: %v", err)
	}

	log.Infof("  write_file: %s (%d bytes)", args.Path, len(args.Content))
	return fmt.Sprintf("wrote %s (%d bytes)", args.Path, len(args.Content))
}

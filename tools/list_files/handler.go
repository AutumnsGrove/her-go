// Package list_files implements the list_files tool — lists files in the
// reports directory with sizes and modification times. Available to both
// the worker agent (to check what already exists) and the driver agent
// (to browse available reports for the user).
package list_files

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/list_files")

func init() {
	tools.Register("list_files", Handle)
}

// Handle lists files in the reports directory, optionally filtered by a
// glob pattern. Returns a formatted list with sizes and dates.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		Pattern string `json:"pattern"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "error: " + err.Error()
	}
	if ctx.ReportsDir == "" {
		return "error: reports directory not configured"
	}

	// Default to listing everything. Note: Go's filepath.Match doesn't
	// support ** globbing — we treat "**/*" as "match all" and fall back
	// to filename-only matching for other patterns.
	if args.Pattern == "" {
		args.Pattern = "**/*"
	}

	var files []string
	err := filepath.WalkDir(ctx.ReportsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			return nil
		}
		// Skip .gitkeep
		if d.Name() == ".gitkeep" {
			return nil
		}

		rel, _ := filepath.Rel(ctx.ReportsDir, path)

		// Apply glob filter if specified.
		if args.Pattern != "**/*" {
			matched, matchErr := filepath.Match(args.Pattern, rel)
			if matchErr != nil || !matched {
				// Also try matching just the filename for simple patterns.
				matched, _ = filepath.Match(args.Pattern, filepath.Base(rel))
				if !matched {
					return nil
				}
			}
		}

		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		files = append(files, fmt.Sprintf("  %s  (%s, %s)",
			rel,
			humanSize(info.Size()),
			info.ModTime().Format(time.RFC3339[:10]),
		))
		return nil
	})
	if err != nil {
		return fmt.Sprintf("error listing files: %v", err)
	}

	if len(files) == 0 {
		return "No files found in reports/."
	}

	log.Infof("  list_files: %d files", len(files))
	return fmt.Sprintf("Reports (%d files):\n%s", len(files), strings.Join(files, "\n"))
}

func humanSize(bytes int64) string {
	switch {
	case bytes >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(1<<20))
	case bytes >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(bytes)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

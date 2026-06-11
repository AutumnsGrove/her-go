// Package publish_report implements the publish_report tool — publishes a
// report file to Telegraph for rich Instant View rendering. Returns the
// public URL so the user can read the report in a formatted view.
package publish_report

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"her/logger"
	"her/telegraph"
	"her/tools"
)

var log = logger.WithPrefix("tools/publish_report")

func init() {
	tools.Register("publish_report", Handle)
}

// Handle reads a report from the reports directory and publishes it to
// Telegraph, returning the URL.
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

	token := ""
	if ctx.Cfg != nil {
		token = ctx.Cfg.WorkerAgent.TelegraphToken
	}
	if token == "" {
		return "error: Telegraph not configured (no telegraph_token in config)"
	}

	absPath, err := tools.ValidateReportPath(ctx.ReportsDir, args.Path)
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}

	content, err := os.ReadFile(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Sprintf("error: report not found: %s", args.Path)
		}
		return fmt.Sprintf("error reading report: %v", err)
	}

	// Extract title from first heading.
	title := args.Path
	for _, line := range strings.Split(string(content), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# ") {
			title = strings.TrimSpace(strings.TrimPrefix(trimmed, "# "))
			break
		}
	}

	authorName := "Mira"
	if ctx.Cfg != nil {
		authorName = ctx.Cfg.Identity.Her
	}

	tc := telegraph.NewClient(token, authorName)
	url, err := tc.CreatePage(title, string(content))
	if err != nil {
		return fmt.Sprintf("error publishing to Telegraph: %v", err)
	}

	// Store the URL so the bot layer auto-appends it after the reply.
	ctx.PublishedReportURL = url

	log.Infof("  publish_report: %s → %s", args.Path, url)
	return fmt.Sprintf("published: %s", url)
}

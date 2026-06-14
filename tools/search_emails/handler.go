// Package search_emails implements the search_emails tool — searches
// the connected Gmail account and returns email summaries. The worker
// agent uses this to scan the inbox before deciding which emails to
// read in full via read_email.
package search_emails

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/search_emails")

func init() {
	tools.Register("search_emails", Handle)
}

// Handle searches Gmail and returns formatted summaries.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		Query string `json:"query"`
		Page  int    `json:"page"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "error: " + err.Error()
	}
	if ctx.GmailBridge == nil {
		return "error: email not configured (no Gmail credentials in config)"
	}
	if args.Page <= 0 {
		args.Page = 1
	}

	label := args.Query
	if label == "" {
		label = "(recent)"
	}
	log.Infof("  search_emails: %s page %d", label, args.Page)

	result, err := ctx.GmailBridge.Search(context.Background(), args.Query, args.Page)
	if err != nil {
		log.Warn("email search failed", "query", args.Query, "err", err)
		return "error: " + err.Error()
	}

	if len(result.Messages) == 0 {
		return "No emails found."
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%d email(s) found", len(result.Messages))
	if result.HasMore {
		sb.WriteString(" (more available, increment page)")
	}
	sb.WriteString(":\n\n")

	for _, msg := range result.Messages {
		unread := ""
		if msg.Unread {
			unread = " [UNREAD]"
		}
		fmt.Fprintf(&sb, "ID: %s%s\nFrom: %s\nSubject: %s\nDate: %s\nSnippet: %s\n\n",
			msg.ID, unread,
			msg.From,
			msg.Subject,
			msg.Date.Format("2006-01-02 15:04"),
			msg.Snippet,
		)
	}

	return sb.String()
}

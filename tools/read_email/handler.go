// Package read_email implements the read_email tool — fetches the full
// content of a single email by message ID. The worker agent uses this
// after search_emails to read emails that look important.
package read_email

import (
	"encoding/json"
	"fmt"
	"strings"

	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/read_email")

func init() {
	tools.Register("read_email", Handle)
}

// Handle fetches a single email by ID and returns its full content.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "error: " + err.Error()
	}
	if ctx.GmailBridge == nil {
		return "error: email not configured (no Gmail credentials in config)"
	}
	if args.ID == "" {
		return "error: id is required"
	}

	log.Infof("  read_email: %s", args.ID)

	msg, err := ctx.GmailBridge.Read(args.ID)
	if err != nil {
		log.Warn("email read failed", "id", args.ID, "err", err)
		return "error: " + err.Error()
	}

	var sb strings.Builder
	unread := ""
	if msg.Unread {
		unread = " [UNREAD]"
	}
	fmt.Fprintf(&sb, "From: %s\nTo: %s\nSubject: %s\nDate: %s%s\n",
		msg.From, msg.To, msg.Subject,
		msg.Date.Format("2006-01-02 15:04"),
		unread,
	)

	if len(msg.Attachments) > 0 {
		fmt.Fprintf(&sb, "Attachments: %s\n", strings.Join(msg.Attachments, ", "))
	}

	sb.WriteString("\n")
	body := msg.Body
	if len(body) > 8000 {
		body = body[:8000] + "\n\n[truncated — email body exceeds 8000 characters]"
	}
	sb.WriteString(body)

	return sb.String()
}

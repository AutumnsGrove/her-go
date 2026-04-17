package layers

// Agent layer: Recent conversation history.
// Gives the agent context for resolving references like "it",
// "that book", "the thing we talked about". The agent sees the
// sliding window of recent messages (typically 10).

import (
	"fmt"
	"strings"
	"time"
)

func init() {
	Register(PromptLayer{
		Name:    "Recent Conversation",
		Order:   200,
		Stream:  StreamAgent,
		Builder: buildAgentHistory,
	})
}

func buildAgentHistory(ctx *LayerContext) LayerResult {
	if len(ctx.RecentMessages) == 0 {
		return LayerResult{}
	}

	var b strings.Builder
	b.WriteString("## Recent Conversation\n\n")

	// prevDay tracks the calendar date so we can detect day boundaries.
	var prevDay time.Time

	for _, msg := range ctx.RecentMessages {
		msgDate := time.Date(
			msg.Timestamp.Year(), msg.Timestamp.Month(), msg.Timestamp.Day(),
			0, 0, 0, 0, msg.Timestamp.Location(),
		)
		if !prevDay.IsZero() && !msgDate.Equal(prevDay) {
			b.WriteString("--- the above messages are from a previous day ---\n\n")
		}
		prevDay = msgDate

		role := ctx.Cfg.Identity.User
		if msg.Role == "assistant" {
			role = ctx.Cfg.Identity.Her
		}
		content := msg.ContentScrubbed
		if content == "" {
			content = msg.ContentRaw
		}
		fmt.Fprintf(&b, "**%s:** %s\n\n", role, content)
	}

	return LayerResult{
		Content: b.String(),
		Detail:  fmt.Sprintf("%d msgs", len(ctx.RecentMessages)),
	}
}

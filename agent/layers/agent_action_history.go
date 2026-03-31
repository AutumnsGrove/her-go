package layers

// Agent layer: Action history — what the agent did in previous turns.
// This gives the agent persistent memory of its own tool calls across turns.
// Without this, the agent has no record of facts it saved, searches it ran,
// reminders it set, etc. once those turns scroll out of the message window.
//
// Two parts:
//   1. Running summary of older actions (from agent compaction)
//   2. Recent actions in full fidelity (tool name + args + result)
//
// Verbose tool results (web_search, book_search, etc.) are truncated —
// the agent only needs to know it searched for X and found Y, not the
// full search result page.

import (
	"fmt"
	"her/compact"
	"strings"
)

func init() {
	Register(PromptLayer{
		Name:    "Action History",
		Order:   150, // after agent prompt (10), tools (50), and time (100); before conversation history (200)
		Stream:  StreamAgent,
		Builder: buildAgentActionHistory,
	})
}

func buildAgentActionHistory(ctx *LayerContext) LayerResult {
	if ctx.AgentActionSummary == "" && len(ctx.RecentAgentActions) == 0 {
		return LayerResult{
			Content: "## Previous Actions\n\n(no previous actions recorded — this is the start of your action history)",
			Detail:  "empty",
		}
	}

	var b strings.Builder
	b.WriteString("## Previous Actions\n\n")
	b.WriteString("This is your action history from previous turns. Use it to avoid repeating work, ")
	b.WriteString("build on past decisions, and correct earlier mistakes (e.g., update a fact you saved incorrectly).\n\n")

	if ctx.AgentActionSummary != "" {
		fmt.Fprintf(&b, "### Summary of Older Actions\n%s\n\n", ctx.AgentActionSummary)
	}

	if len(ctx.RecentAgentActions) > 0 {
		b.WriteString("### Recent Actions (full detail)\n\n")
		for _, a := range ctx.RecentAgentActions {
			result := a.Result
			// Truncate verbose tool results — the agent doesn't need
			// full search results from previous turns.
			if compact.VerboseTools[a.ToolName] && len(result) > 200 {
				result = result[:200] + "... (truncated)"
			}
			args := a.ToolArgs
			if len(args) > 300 {
				args = args[:300] + "..."
			}
			fmt.Fprintf(&b, "- **%s**(%s) → %s\n", a.ToolName, args, truncateResult(result, 300))
		}
	}

	return LayerResult{
		Content: b.String(),
		Detail:  fmt.Sprintf("%d recent + summary", len(ctx.RecentAgentActions)),
	}
}

// truncateResult shortens a tool result for display, collapsing newlines.
func truncateResult(s string, maxLen int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

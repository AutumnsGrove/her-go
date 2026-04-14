package layers

// Layer 5: Mood awareness.
// Recent emotional states so the bot can be attentive to patterns.
// Only included when mood data exists in the DB.

import (
	"fmt"
	"strings"
)

func init() {
	Register(PromptLayer{
		Name:    "Mood Trend",
		Order:   500,
		Stream:  StreamChat,
		Builder: buildChatMood,
	})
}

func buildChatMood(ctx *LayerContext) LayerResult {
	if ctx.Store == nil {
		return LayerResult{}
	}

	entries, err := ctx.Store.RecentMoodEntries(5)
	if err != nil || len(entries) == 0 {
		return LayerResult{}
	}

	labels := map[int]string{1: "bad", 2: "rough", 3: "meh", 4: "good", 5: "great"}

	var b strings.Builder
	b.WriteString("# Mood Awareness\n\n")
	b.WriteString("Recent emotional states (use this to be attentive, not to announce it):\n\n")

	for _, e := range entries {
		label := labels[e.Rating]
		if label == "" {
			label = "unknown"
		}
		ts := e.Timestamp.Format("Mon Jan 2, 3:04 PM")
		if e.Note != "" {
			fmt.Fprintf(&b, "- %s: %s (%d/5) — %s\n", ts, label, e.Rating, e.Note)
		} else {
			fmt.Fprintf(&b, "- %s: %s (%d/5)\n", ts, label, e.Rating)
		}
	}

	avg, count, err := ctx.Store.MoodTrend(10)
	if err == nil && count >= 3 {
		var trend string
		switch {
		case avg >= 4.0:
			trend = "trending positive"
		case avg >= 3.0:
			trend = "mostly neutral"
		case avg >= 2.0:
			trend = "trending down"
		default:
			trend = "going through a rough patch"
		}
		fmt.Fprintf(&b, "\nOverall trend (last %d entries): %.1f/5 — %s\n", count, avg, trend)
	}

	content := b.String()
	return LayerResult{
		Content: content,
		Detail:  fmt.Sprintf("%d entries", len(entries)),
	}
}

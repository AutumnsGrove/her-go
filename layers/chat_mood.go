package layers

// Layer 5: Mood context — recent mood snapshots injected into the chat
// model's prompt so replies can acknowledge the user's state of mind
// without being coached on it every turn.
//
// Shows:
//   - latest daily rollup (if any from the last 48h)
//   - up to 5 most recent momentary entries
//
// Entries with source=manual or source=confirmed are labelled "self-
// reported" to help the model calibrate trust. Inferred entries carry
// their confidence. Layer emits nothing when there's no mood history
// (first-run or pure-factual-usage users).

import (
	"fmt"
	"strings"
	"time"

	"her/logger"
	"her/memory"
)

var log = logger.WithPrefix("layers")

// minMoodEntriesToInject is the threshold below which the layer emits
// nothing. A single stray inference out of context is noise, not
// signal — wait until there's a pattern before telling the model.
const minMoodEntriesToInject = 2

func init() {
	Register(PromptLayer{
		Name:    "Mood Context",
		Order:   500,
		Stream:  StreamChat,
		Builder: buildChatMood,
	})
}

func buildChatMood(ctx *LayerContext) LayerResult {
	if ctx.Store == nil {
		return LayerResult{}
	}

	recent, err := ctx.Store.RecentMoodEntries(memory.MoodKindMomentary, 5)
	if err != nil {
		log.Debug("mood layer: failed to load recent entries", "err", err)
		return LayerResult{}
	}
	latestDaily, err := ctx.Store.LatestMoodEntry(memory.MoodKindDaily)
	if err != nil {
		log.Debug("mood layer: failed to load daily entry", "err", err)
	}

	totalEntries := len(recent)
	if latestDaily != nil && daysSince(latestDaily.Timestamp) <= 2 {
		totalEntries++
	}
	if totalEntries < minMoodEntriesToInject {
		return LayerResult{}
	}

	var b strings.Builder
	b.WriteString("## Recent mood\n\n")

	if latestDaily != nil && daysSince(latestDaily.Timestamp) <= 2 {
		fmt.Fprintf(&b, "- %s (daily rollup): valence %d/7%s%s\n",
			humanTime(latestDaily.Timestamp),
			latestDaily.Valence,
			renderLabels(latestDaily.Labels),
			renderAssocs(latestDaily.Associations),
		)
	}

	for _, e := range recent {
		fmt.Fprintf(&b, "- %s: valence %d/7%s%s%s\n",
			humanTime(e.Timestamp),
			e.Valence,
			renderLabels(e.Labels),
			renderAssocs(e.Associations),
			sourceTag(e),
		)
	}

	b.WriteString("\nAcknowledge this gently when appropriate — don't lecture or make it the focus of every reply. Inferred entries may be wrong; don't take them as settled fact.\n")

	return LayerResult{
		Content: b.String(),
		Detail:  fmt.Sprintf("%d recent mood entries", totalEntries),
	}
}

// renderLabels formats the labels slice as ", labels: X, Y" or empty
// when there are none. Keeps the block compact.
func renderLabels(labels []string) string {
	if len(labels) == 0 {
		return ""
	}
	return ", labels: " + strings.Join(labels, ", ")
}

// renderAssocs formats associations similarly to renderLabels.
func renderAssocs(assocs []string) string {
	if len(assocs) == 0 {
		return ""
	}
	return ", re: " + strings.Join(assocs, ", ")
}

// sourceTag adds a trust-calibration hint — confirmed/manual entries
// are self-reported; inferred entries carry their confidence so the
// model can weight them accordingly.
func sourceTag(e memory.MoodEntry) string {
	switch e.Source {
	case memory.MoodSourceConfirmed, memory.MoodSourceManual:
		return " (self-reported)"
	case memory.MoodSourceInferred:
		return fmt.Sprintf(" (inferred, confidence %.2f)", e.Confidence)
	default:
		return ""
	}
}

// humanTime renders a timestamp relative to now when recent, absolute
// when older. Keeps the layer readable without being precise.
func humanTime(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < 1*time.Hour:
		m := int(d.Minutes())
		if m <= 1 {
			return "just now"
		}
		return fmt.Sprintf("%d min ago", m)
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hr ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	default:
		return t.Format("Jan 2")
	}
}

// daysSince is a convenience for threshold checks ("show the daily
// rollup only if less than N days old").
func daysSince(t time.Time) int {
	return int(time.Since(t).Hours() / 24)
}

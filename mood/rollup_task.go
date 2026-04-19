package mood

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"her/memory"
	"her/scheduler"
)

// The daily rollup is a scheduler extension. It runs at the time
// declared in mood/task.yaml (default 21:00 local) and produces one
// kind=daily, source=inferred row summarizing the day's momentary
// entries. If a Telegram send callback is wired in Deps, it also
// sends the user a quick summary message.
//
// Algorithmic rollup — no LLM call needed. The aggregation is:
//
//	valence       — mean of today's momentary entries, rounded 1..7
//	labels        — top 3 by frequency across today's entries
//	associations  — top 1 by frequency
//	note          — simple template built from the above
//
// See docs/plans/PLAN-mood-tracking-redesign.md § Daily rollup.

const dailyRollupKind = "mood_daily_rollup"

// dailyRollupHandler implements scheduler.Handler. Registered at
// init() time below so the scheduler picks it up automatically.
type dailyRollupHandler struct{}

func (dailyRollupHandler) Kind() string       { return dailyRollupKind }
func (dailyRollupHandler) ConfigPath() string { return "mood/task.yaml" }

func (h dailyRollupHandler) Execute(
	ctx context.Context,
	_ json.RawMessage,
	deps *scheduler.Deps,
) error {
	store, ok := deps.Store.(*memory.Store)
	if !ok {
		return fmt.Errorf("mood_daily_rollup: deps.Store is %T, want *memory.Store", deps.Store)
	}

	now := time.Now()
	start := startOfDay(now)

	entries, err := store.MoodEntriesInRange(memory.MoodKindMomentary, start, now)
	if err != nil {
		return fmt.Errorf("mood_daily_rollup: fetching entries: %w", err)
	}

	// Dedup guard: if today already has a daily entry, don't create
	// another one on a re-run (e.g. scheduler restart inside the
	// window).
	existingDaily, err := store.MoodEntriesInRange(memory.MoodKindDaily, start, now)
	if err != nil {
		return fmt.Errorf("mood_daily_rollup: checking existing daily: %w", err)
	}
	if len(existingDaily) > 0 {
		log.Info("mood daily rollup: entry already exists for today; skipping",
			"existing_id", existingDaily[0].ID)
		return nil
	}

	if len(entries) == 0 {
		log.Info("mood daily rollup: no momentary entries today; skipping")
		return nil
	}

	draft := computeDailyRollup(entries, now)

	id, err := store.SaveMoodEntry(draft)
	if err != nil {
		return fmt.Errorf("mood_daily_rollup: saving entry: %w", err)
	}
	draft.ID = id
	log.Info("mood daily rollup logged",
		"id", id, "valence", draft.Valence,
		"labels", draft.Labels, "n_entries", len(entries))

	// Optional: push a short summary to the owner chat.
	if deps.Send != nil && deps.ChatID != 0 {
		text := formatRollupSummary(draft, len(entries))
		if _, err := deps.Send(deps.ChatID, text); err != nil {
			log.Warn("mood daily rollup: Telegram send failed", "err", err)
		}
	}

	_ = ctx // reserved for future cancellation propagation
	return nil
}

// computeDailyRollup aggregates a day's momentary entries into one
// daily MoodEntry. Deterministic — same input → same output — so
// sim runs are reproducible.
func computeDailyRollup(entries []memory.MoodEntry, at time.Time) *memory.MoodEntry {
	// Mean valence, rounded to nearest int in [1,7].
	sum := 0
	for _, e := range entries {
		sum += e.Valence
	}
	meanF := float64(sum) / float64(len(entries))
	valence := int(meanF + 0.5)
	if valence < 1 {
		valence = 1
	} else if valence > 7 {
		valence = 7
	}

	// Top 3 labels by frequency, then alphabetical to break ties.
	labelCounts := map[string]int{}
	for _, e := range entries {
		for _, l := range e.Labels {
			labelCounts[l]++
		}
	}
	topLabels := topNByCount(labelCounts, 3)

	// Top association (single).
	assocCounts := map[string]int{}
	for _, e := range entries {
		for _, a := range e.Associations {
			assocCounts[a]++
		}
	}
	topAssocs := topNByCount(assocCounts, 1)

	note := buildRollupNote(valence, topLabels, topAssocs, len(entries))

	// Conversation ID is empty for rollups — the entry spans the day.
	return &memory.MoodEntry{
		Timestamp:    at,
		Kind:         memory.MoodKindDaily,
		Valence:      valence,
		Labels:       topLabels,
		Associations: topAssocs,
		Note:         note,
		Source:       memory.MoodSourceInferred,
		Confidence:   1.0, // algorithmic, not a guess
	}
}

// topNByCount returns the top n keys of counts, sorted by count
// descending then key ascending (stable tie-break).
func topNByCount(counts map[string]int, n int) []string {
	type kv struct {
		k string
		v int
	}
	items := make([]kv, 0, len(counts))
	for k, v := range counts {
		items = append(items, kv{k, v})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].v != items[j].v {
			return items[i].v > items[j].v
		}
		return items[i].k < items[j].k
	})
	if n > len(items) {
		n = len(items)
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, items[i].k)
	}
	return out
}

// buildRollupNote renders the auto-note as one short sentence. Kept
// deliberately simple — no LLM call, no creative language. When we
// want richer narrative, switch to an LLM-based summarizer later.
func buildRollupNote(valence int, labels, assocs []string, n int) string {
	var b strings.Builder
	switch {
	case valence <= 3:
		b.WriteString("Unpleasant day")
	case valence == 4:
		b.WriteString("Neutral day")
	default:
		b.WriteString("Pleasant day")
	}
	if len(labels) > 0 {
		fmt.Fprintf(&b, " — mostly %s", strings.ToLower(strings.Join(labels, ", ")))
	}
	if len(assocs) > 0 {
		fmt.Fprintf(&b, ", around %s", strings.ToLower(assocs[0]))
	}
	fmt.Fprintf(&b, ". (%d check-ins)", n)
	return b.String()
}

// formatRollupSummary renders the daily rollup for a Telegram send.
func formatRollupSummary(e *memory.MoodEntry, n int) string {
	header := "🌙 today's mood rollup"
	body := fmt.Sprintf("%s\n\nvalence: %d/7 (%s)\nlabels: %s\nassoc: %s\n\n%s",
		header,
		e.Valence,
		valenceWord(e.Valence),
		orDash(strings.Join(e.Labels, ", ")),
		orDash(strings.Join(e.Associations, ", ")),
		e.Note,
	)
	_ = n // already inlined in Note
	return body
}

func valenceWord(v int) string {
	switch {
	case v <= 3:
		return "unpleasant"
	case v == 4:
		return "neutral"
	default:
		return "pleasant"
	}
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// startOfDay returns t truncated to 00:00 in t's own location. Using
// the timestamp's local zone — not UTC — matches how users experience
// "today" relative to their actual wall clock.
func startOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// DailyRollupHandler returns a scheduler.Handler for the daily mood
// rollup. Mostly useful for the sim (which calls Execute directly to
// force-run the handler without waiting for 21:00); production code
// goes through the init()-registered instance.
func DailyRollupHandler() scheduler.Handler {
	return dailyRollupHandler{}
}

func init() {
	scheduler.Register(dailyRollupHandler{})
}

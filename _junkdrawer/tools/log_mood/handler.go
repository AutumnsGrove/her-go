// Package log_mood implements the log_mood tool — saves a mood data point
// to the mood_entries table.
//
// This was previously a skill (subprocess via DB proxy). It's now a
// first-class tool because it only does a DB insert — no network access,
// no sandbox needed.
//
// Includes two quality gates before writing:
//  1. Dedup gate — rejects if a mood was logged in the last 30 minutes
//     (time gate) or a semantically similar mood in the last 2 hours.
//  2. Classifier gate — validates the mood is about the real user, not
//     a fictional character or game event.
package log_mood

import (
	"encoding/json"
	"fmt"

	"her/embed"
	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/log_mood")

// moodDedupWindowMinutes is how far back the semantic gate looks. 120
// minutes means if the user said "feeling stuck" an hour ago, we won't
// log "feeling stuck and restless" as a separate entry.
const moodDedupWindowMinutes = 120

// moodSimilarityThreshold is the cosine similarity above which two mood
// notes are considered duplicates. Lower than the fact threshold (0.85)
// because mood notes are short and similar phrasing should be caught.
const moodSimilarityThreshold = 0.75

// moodLabels maps rating integers to human-readable labels.
var moodLabels = map[int]string{1: "bad", 2: "rough", 3: "meh", 4: "good", 5: "great"}

func init() {
	tools.Register("log_mood", Handle)
}

// Handle validates the mood, runs quality gates, and saves to the DB.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		Rating int    `json:"rating"`
		Note   string `json:"note"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	if args.Rating < 1 || args.Rating > 5 {
		return fmt.Sprintf("error: rating must be 1-5, got %d", args.Rating)
	}
	if args.Note == "" {
		return "error: note is required"
	}

	if ctx.Store == nil {
		return "error: database not available"
	}

	// --- Dedup gate ---
	// Two tiers: time gate (any mood in last 30 min) and semantic gate
	// (similar note in last 2 hours). Prevents the agent from logging
	// the same mood on every message in an emotional conversation.
	if ctx.EmbedClient != nil {
		if dup := checkDuplicate(args.Note, ctx); dup != "" {
			return dup
		}
	}

	// --- Classifier gate ---
	// Validates the mood is about the real user, not a fictional character.
	// Runs after dedup (no point classifying a duplicate) and before the
	// DB write. Fail-open: if classifier is nil, writes proceed.
	if ctx.ClassifierLLM != nil && ctx.ClassifyWriteFunc != nil {
		snippet, _ := ctx.Store.RecentMessages(ctx.ConversationID, 3)
		verdict := ctx.ClassifyWriteFunc("mood", args.Note, snippet)
		if !verdict.Allowed {
			if ctx.RejectionMessageFunc != nil {
				return ctx.RejectionMessageFunc(verdict)
			}
			return fmt.Sprintf("rejected by classifier: %s", verdict.Reason)
		}
	}

	// --- Save ---
	id, err := ctx.Store.SaveMoodEntry(args.Rating, args.Note, "", "manual", ctx.ConversationID)
	if err != nil {
		log.Error("saving mood", "err", err)
		return fmt.Sprintf("error saving mood: %v", err)
	}

	label := moodLabels[args.Rating]
	log.Infof("  log_mood: %d/5 (%s) — %s → ID=%d", args.Rating, label, args.Note, id)

	return fmt.Sprintf("mood logged: %d/5 (%s) — %s", args.Rating, label, args.Note)
}

// checkDuplicate implements the two-tier dedup gate. Returns an
// explanatory string if the mood should be skipped, or "" if OK.
//
// Tier 1 (time gate): any mood in the last 30 minutes → skip.
// Tier 2 (semantic gate): similar note in the last 2 hours → skip.
func checkDuplicate(note string, ctx *tools.Context) string {
	// Tier 1: time gate.
	recentNotes, err := ctx.Store.RecentMoodNotes(30)
	if err != nil {
		log.Warn("mood dedup: couldn't check recent moods", "err", err)
		return "" // fail open
	}
	if len(recentNotes) > 0 {
		log.Info("mood dedup: skipped (mood already logged in last 30 min)",
			"recent_note", recentNotes[0], "proposed_note", note)
		return fmt.Sprintf(
			"mood already logged in the last 30 minutes (%q) — "+
				"if the mood has genuinely shifted, use update_mood instead "+
				"to update the existing entry", recentNotes[0])
	}

	// Tier 2: semantic gate.
	if note == "" {
		return ""
	}
	windowNotes, err := ctx.Store.RecentMoodNotes(moodDedupWindowMinutes)
	if err != nil {
		log.Warn("mood dedup: couldn't check window moods", "err", err)
		return ""
	}
	if len(windowNotes) == 0 {
		return ""
	}

	newVec, err := ctx.EmbedClient.Embed(note)
	if err != nil {
		log.Warn("mood dedup: embed failed", "err", err)
		return "" // fail open
	}

	for _, existing := range windowNotes {
		existVec, err := ctx.EmbedClient.Embed(existing)
		if err != nil {
			continue
		}
		sim := embed.CosineSimilarity(newVec, existVec)
		if sim >= moodSimilarityThreshold {
			log.Info("mood dedup: skipped (semantically similar)",
				"similarity", fmt.Sprintf("%.3f", sim),
				"existing", existing, "proposed", note)
			return fmt.Sprintf("mood too similar to recent entry %q (%.0f%% match) — skipping",
				existing, sim*100)
		}
	}

	return ""
}

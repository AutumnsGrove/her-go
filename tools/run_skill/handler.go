// Package run_skill implements the run_skill tool — executes a skill by name
// with given arguments via the skills/loader package.
//
// The skill binary receives args as JSON on stdin and writes its result to
// stdout. The runner handles compilation (for Go skills), timeouts, and
// environment sandboxing.
//
// Special handling for log_mood: applies a two-tier dedup gate (time gate
// + semantic gate) and a classifier check before executing. This prevents
// the agent from logging the same mood multiple times in one conversation.
package run_skill

import (
	"encoding/json"
	"fmt"
	"strings"

	"her/embed"
	"her/logger"
	"her/skills/loader"
	"her/tools"
)

var log = logger.WithPrefix("tools/run_skill")

// moodDedupWindowMinutes is how far back we look for duplicate moods.
// 120 minutes (2 hours) means: if the user said "feeling stuck" an hour
// ago, we won't log "feeling stuck and restless" as a separate entry.
const moodDedupWindowMinutes = 120

// moodSimilarityThreshold is the cosine similarity above which two mood
// notes are considered duplicates. 0.75 is intentionally lower than the
// fact threshold (0.85) because mood notes tend to be short and similar
// phrasing should be caught more aggressively.
const moodSimilarityThreshold = 0.75

func init() {
	tools.Register("run_skill", Handle)
}

// Handle looks up the skill by name, runs the dedup/classifier gates if
// it's log_mood, then executes the skill and returns its output.
func Handle(argsJSON string, ctx *tools.Context) string {
	if ctx.SkillRegistry == nil {
		return "no skills available — skill registry not initialized"
	}

	var args struct {
		Name string         `json:"name"`
		Args map[string]any `json:"args"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing run_skill args: %s", err)
	}
	if args.Name == "" {
		return "error: skill name is required"
	}

	// Look up the skill.
	skill := ctx.SkillRegistry.Get(args.Name)
	if skill == nil {
		// Suggest using find_skill — a common mistake is to guess the name.
		available := ctx.SkillRegistry.List()
		if len(available) > 0 {
			return fmt.Sprintf("unknown skill %q. Available: %s. Use find_skill to search by intent.",
				args.Name, strings.Join(available, ", "))
		}
		return fmt.Sprintf("unknown skill %q — no skills are registered", args.Name)
	}

	// Default to empty args if none provided.
	if args.Args == nil {
		args.Args = make(map[string]any)
	}

	// --- Mood dedup gate ---
	// Before running log_mood, check if a semantically similar mood was
	// logged recently. This mirrors how save_fact rejects duplicate facts
	// via embedding similarity. Without this, the agent tends to log the
	// same mood on every message in an emotional conversation.
	if skill.Name == "log_mood" && ctx.Store != nil && ctx.EmbedClient != nil {
		if dup := checkMoodDuplicate(args.Args, ctx); dup != "" {
			return dup
		}
	}

	// --- Classifier gate for mood ---
	// Check if the mood is about the real user or a fictional character.
	// Runs after dedup (no point classifying a duplicate) and before
	// execution (we can't intercept the DB write inside the skill).
	if skill.Name == "log_mood" && ctx.ClassifierLLM != nil && ctx.ClassifyWriteFunc != nil {
		moodNote, _ := args.Args["note"].(string)
		if moodNote != "" {
			snippet, _ := ctx.Store.RecentMessages(ctx.ConversationID, 3)
			verdict := ctx.ClassifyWriteFunc("mood", moodNote, snippet)
			if !verdict.Allowed {
				if ctx.RejectionMessageFunc != nil {
					return ctx.RejectionMessageFunc(verdict)
				}
				return fmt.Sprintf("rejected by classifier: %s", verdict.Reason)
			}
		}
	}

	// Update status so the user sees what's happening.
	if ctx.StatusCallback != nil {
		ctx.StatusCallback(fmt.Sprintf("running %s...", skill.Name))
	}

	log.Info("running skill", "name", skill.Name)
	result, err := loader.Run(skill, args.Args)
	if err != nil {
		return fmt.Sprintf("error running skill %s: %s", skill.Name, err)
	}

	log.Info("skill finished", "name", skill.Name, "duration", result.Duration)

	// Format the result for the agent.
	if result.Error != "" {
		if result.Output != nil {
			// Skill wrote error JSON before exiting (via skillkit.Error).
			return fmt.Sprintf("skill %s error: %s\noutput: %s", skill.Name, result.Error, string(result.Output))
		}
		return fmt.Sprintf("skill %s error: %s", skill.Name, result.Error)
	}

	if result.Output != nil {
		return string(result.Output)
	}
	if result.RawOut != "" {
		return fmt.Sprintf("(non-JSON output) %s", result.RawOut)
	}

	return "skill completed with no output"
}

// checkMoodDuplicate checks whether a proposed mood entry is too similar to
// a recently logged one. Returns an explanatory string if the mood should be
// skipped, or an empty string if it's OK to log.
//
// Two-tier check:
//  1. Time gate — any mood in the last 30 minutes is a duplicate regardless
//     of content (the user's emotional state doesn't change that fast).
//  2. Semantic gate — any mood in the last 2 hours with a similar note
//     (cosine similarity >= 0.75) is a duplicate.
func checkMoodDuplicate(skillArgs map[string]any, ctx *tools.Context) string {
	note, _ := skillArgs["note"].(string)

	// Tier 1: time gate — if ANY mood was logged in the last 30 minutes,
	// skip this one. The agent shouldn't log mood multiple times per exchange.
	recentNotes, err := ctx.Store.RecentMoodNotes(30)
	if err != nil {
		log.Warn("mood dedup: couldn't check recent moods", "err", err)
		return "" // fail open — let it through
	}
	if len(recentNotes) > 0 {
		log.Info("mood dedup: skipped (mood already logged in last 30 min)",
			"recent_note", recentNotes[0], "proposed_note", note)
		return fmt.Sprintf("mood already logged in the last 30 minutes (%q) — if the mood has genuinely shifted, use update_mood instead to update the existing entry", recentNotes[0])
	}

	// Tier 2: semantic gate — check the last 2 hours for similar notes.
	if note == "" {
		return "" // no note to compare
	}
	windowNotes, err := ctx.Store.RecentMoodNotes(moodDedupWindowMinutes)
	if err != nil {
		log.Warn("mood dedup: couldn't check window moods", "err", err)
		return ""
	}
	if len(windowNotes) == 0 {
		return "" // no moods in window — all clear
	}

	// Embed the proposed note and compare against each recent note.
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
			log.Info("mood dedup: skipped (semantically similar to recent mood)",
				"similarity", fmt.Sprintf("%.3f", sim),
				"existing", existing, "proposed", note)
			return fmt.Sprintf("mood too similar to recent entry %q (%.0f%% match) — skipping", existing, sim*100)
		}
	}

	return "" // no duplicates found
}

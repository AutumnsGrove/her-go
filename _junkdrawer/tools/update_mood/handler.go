// Package update_mood implements the update_mood tool — overwrites the most
// recent mood entry when the user's emotional state shifts within the dedup
// window.
//
// This exists because log_mood has a 30-minute cooldown. When mood shifts
// within that window (e.g., hopeful → stressed after bad news), the agent
// updates the existing entry rather than being blocked entirely.
package update_mood

import (
	"encoding/json"
	"fmt"

	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/update_mood")

// moodLabels maps rating integers to human-readable labels.
var moodLabels = map[int]string{1: "bad", 2: "rough", 3: "meh", 4: "good", 5: "great"}

func init() {
	tools.Register("update_mood", Handle)
}

// Handle finds the most recent mood entry and updates its rating and note.
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

	// Find the most recent mood entry.
	entry, err := ctx.Store.LatestMoodEntry()
	if err != nil {
		log.Error("querying latest mood", "err", err)
		return fmt.Sprintf("error finding latest mood: %v", err)
	}
	if entry == nil {
		return "no mood entries to update — use log_mood to create one first"
	}

	oldRating := entry.Rating

	// Update it.
	if err := ctx.Store.UpdateMoodEntry(entry.ID, args.Rating, args.Note); err != nil {
		log.Error("updating mood", "err", err)
		return fmt.Sprintf("error updating mood: %v", err)
	}

	log.Infof("  update_mood: %d/5 → %d/5 on entry %d", oldRating, args.Rating, entry.ID)

	return fmt.Sprintf("mood updated: %d/5 (%s) → %d/5 (%s) — %s",
		oldRating, moodLabels[oldRating],
		args.Rating, moodLabels[args.Rating],
		args.Note)
}

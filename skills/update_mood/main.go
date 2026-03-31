// update_mood updates the most recent mood entry instead of creating a new one.
//
// This exists because log_mood has a 30-minute cooldown to prevent chart spam.
// When mood shifts within that window (e.g., hopeful → stressed after bad news),
// the agent can update the existing entry rather than being blocked entirely.
//
// Usage (via harness):
//
//	echo '{"rating":2,"note":"got a rejection email"}' | ./bin/update_mood
package main

import (
	"fmt"
	"strconv"

	"skillkit"
)

// Args defines the parameters this skill accepts.
type Args struct {
	Rating int    `json:"rating" flag:"rating" desc:"New mood rating 1-5"`
	Note   string `json:"note" flag:"note" desc:"Updated context for the rating"`
}

func main() {
	var args Args
	skillkit.ParseArgs(&args)

	if args.Rating < 1 || args.Rating > 5 {
		skillkit.Error(fmt.Sprintf("rating must be 1-5, got %d", args.Rating))
	}

	db := skillkit.DB()

	// Find the most recent mood entry.
	result, err := db.Query("mood_entries", skillkit.QueryParams{
		Limit: 1,
		Where: "1=1 ORDER BY timestamp DESC",
	})
	if err != nil {
		skillkit.Error("failed to query recent mood: " + err.Error())
	}
	if result.Count == 0 {
		skillkit.Error("no mood entries to update — use log_mood to create one first")
	}

	// Get the ID of the most recent entry. The proxy returns IDs as
	// float64 (JSON numbers) — convert to string for the Update call.
	row := result.Rows[0]
	idFloat, ok := row["id"].(float64)
	if !ok {
		skillkit.Error("unexpected ID type in mood entry")
	}
	id := strconv.FormatInt(int64(idFloat), 10)

	// Grab the old values for the response message.
	oldRating := 0
	if r, ok := row["rating"].(float64); ok {
		oldRating = int(r)
	}
	oldNote, _ := row["note"].(string)

	// Update the entry with new values.
	err = db.Update("mood_entries", id, map[string]any{
		"rating": args.Rating,
		"note":   args.Note,
	})
	if err != nil {
		skillkit.Error("failed to update mood: " + err.Error())
	}

	labels := map[int]string{1: "bad", 2: "rough", 3: "meh", 4: "good", 5: "great"}
	skillkit.Output(map[string]any{
		"id":           int64(idFloat),
		"old_rating":   oldRating,
		"old_note":     oldNote,
		"new_rating":   args.Rating,
		"new_note":     args.Note,
		"message":      fmt.Sprintf("mood updated: %d/5 (%s) → %d/5 (%s) — %s", oldRating, labels[oldRating], args.Rating, labels[args.Rating], args.Note),
	})
}

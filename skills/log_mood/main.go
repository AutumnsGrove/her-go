// log_mood is a skill that logs the user's emotional state to the
// mood_entries table via the DB proxy.
//
// This was the first built-in tool migrated to a standalone skill.
// It demonstrates the full DB proxy pipeline: the harness sets
// DB_PROXY_URL, the skill calls skillkit.DB().Insert(), and the
// proxy writes to her.db with authorizer-enforced access control.
//
// Usage (via harness):
//
//	echo '{"rating":4,"note":"good day"}' | ./bin/log_mood
//
// Usage (manual testing — requires DB_PROXY_URL to be set):
//
//	go run main.go --rating 4 --note "good day"
package main

import (
	"fmt"

	"skillkit"
)

// Args defines the parameters this skill accepts.
type Args struct {
	Rating int    `json:"rating" flag:"rating" desc:"Mood rating 1-5"`
	Note   string `json:"note" flag:"note" desc:"Brief context for the rating"`
}

func main() {
	var args Args
	skillkit.ParseArgs(&args)

	// Validate rating range.
	if args.Rating < 1 || args.Rating > 5 {
		skillkit.Error(fmt.Sprintf("rating must be 1-5, got %d", args.Rating))
	}

	// Insert the mood entry via the DB proxy.
	db := skillkit.DB()
	id, err := db.Insert("mood_entries", map[string]any{
		"rating":          args.Rating,
		"note":            args.Note,
		"source":          "manual",
		"conversation_id": "", // skills don't have conversation context
	})
	if err != nil {
		skillkit.Error("failed to save mood: " + err.Error())
	}

	// Return structured output for the agent.
	labels := map[int]string{1: "bad", 2: "rough", 3: "meh", 4: "good", 5: "great"}
	skillkit.Output(map[string]any{
		"id":      id,
		"rating":  args.Rating,
		"label":   labels[args.Rating],
		"note":    args.Note,
		"message": fmt.Sprintf("mood logged: %d/5 (%s) — %s", args.Rating, labels[args.Rating], args.Note),
	})
}

package scenarios

import (
	"fmt"

	"her/mood"
	"her/sim"
)

// mood_dedup_skips_near_duplicate: the user says essentially the same
// thing twice in quick succession. The first call auto-logs. The
// second call produces the same embedding (sim's deterministic Embed
// returns a constant vector for identical inputs), and the KNN dedup
// pass catches it inside the DedupWindow.

const dedupEmbedDim = 8

func init() {
	sim.RegisterScenario(sim.Scenario{
		Name: "mood_dedup_skips_near_duplicate",
		Description: "Second highly-similar inference within the dedup window is " +
			"skipped rather than written again.",

		HarnessOptions: sim.HarnessOptions{
			EmbedDim: dedupEmbedDim, // turns on vec_moods in the store
		},

		Setup: func(h *sim.Harness) error {
			// Same high-confidence reply twice.
			h.LLM.Script("", `{"skip":false,"valence":2,"labels":["Stressed"],"associations":[],"note":"wiped","confidence":0.9,"signals":[]}`)
			return nil
		},

		Steps: []sim.Step{
			{
				Name: "first turn — auto-logs",
				Do: func(h *sim.Harness) error {
					res := runMood(h, mood.AgentConfig{}, dedupEmbedDim, []mood.Turn{{
						Role:            "user",
						ScrubbedContent: "I'm absolutely exhausted",
					}})
					if res.Action != mood.ActionAutoLogged {
						return fmt.Errorf("first run action = %q, reason: %s", res.Action, res.Reason)
					}
					return nil
				},
			},
			{
				Name: "second turn — dedup catches the near-duplicate",
				Do: func(h *sim.Harness) error {
					res := runMood(h, mood.AgentConfig{}, dedupEmbedDim, []mood.Turn{{
						Role:            "user",
						ScrubbedContent: "I'm absolutely exhausted",
					}})
					if res.Action != mood.ActionDroppedDedup {
						return fmt.Errorf("second run action = %q, reason: %s",
							res.Action, res.Reason)
					}
					return nil
				},
			},
		},

		Assertions: []sim.Assertion{
			{
				Name: "exactly one mood entry was saved (second was dedup-dropped)",
				Check: func(h *sim.Harness) error {
					entries, _ := h.Store.RecentMoodEntries("", 5)
					if len(entries) != 1 {
						return fmt.Errorf("entries = %d, want 1", len(entries))
					}
					return nil
				},
			},
			{
				Name: "no Telegram message was sent (both paths were server-side)",
				Check: func(h *sim.Harness) error {
					if sends := h.Transport.MessagesByKind(sim.EventSend); len(sends) != 0 {
						return fmt.Errorf("sends = %d, want 0", len(sends))
					}
					return nil
				},
			},
		},
	})
}

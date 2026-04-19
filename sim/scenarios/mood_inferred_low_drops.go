package scenarios

import (
	"fmt"

	"her/mood"
	"her/sim"
)

// mood_inferred_low_drops: the LLM returns low confidence AND the
// user's turn has no affect signal. The agent drops the inference
// silently — no DB write, no Telegram send. Protects the user from
// "every factual question becomes a mood" noise.

func init() {
	sim.RegisterScenario(sim.Scenario{
		Name:        "mood_inferred_low_drops",
		Description: "Low-confidence inferred mood is dropped silently — no DB row, no Telegram message.",

		Setup: func(h *sim.Harness) error {
			h.LLM.Script("", `{"skip":false,"valence":4,"labels":["Calm"],"associations":[],"note":"neutral vibe","confidence":0.25,"signals":[]}`)
			return nil
		},

		Steps: []sim.Step{{
			Name: "run mood agent on a factual, affect-free turn",
			Do: func(h *sim.Harness) error {
				res := runMood(h, mood.AgentConfig{}, 0, []mood.Turn{{
					Role:            "user",
					ScrubbedContent: "what's the time zone for utc+2",
				}})
				if res.Action != mood.ActionDroppedLow {
					return fmt.Errorf("mood agent action = %q, reason: %s", res.Action, res.Reason)
				}
				return nil
			},
		}},

		Assertions: []sim.Assertion{
			{
				Name: "no mood entry was saved",
				Check: func(h *sim.Harness) error {
					entries, _ := h.Store.RecentMoodEntries("", 5)
					if len(entries) != 0 {
						return fmt.Errorf("got %d entries, want 0", len(entries))
					}
					return nil
				},
			},
			{
				Name: "no Telegram message was sent",
				Check: func(h *sim.Harness) error {
					events := h.Transport.Events()
					if len(events) != 0 {
						return fmt.Errorf("got %d events, want 0", len(events))
					}
					return nil
				},
			},
		},
	})
}

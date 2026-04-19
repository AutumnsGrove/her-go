package scenarios

import (
	"fmt"

	"her/memory"
	"her/mood"
	"her/sim"
)

// mood_inferred_high_auto_log: the user says something with clearly
// affect-bearing first-person language. The LLM returns high confidence
// (≥ 0.75). The mood agent writes source=inferred to the DB and does
// NOT send a Telegram message — the user never has to confirm.

func init() {
	sim.RegisterScenario(sim.Scenario{
		Name:        "mood_inferred_high_auto_log",
		Description: "High-confidence inferred mood is auto-logged; no Telegram proposal is sent.",

		Setup: func(h *sim.Harness) error {
			// The FakeLLM's scripted reply. Labels are verbatim from
			// the Apple vocab so the vocab filter doesn't drop them.
			h.LLM.Script("", `{"skip":false,"valence":2,"labels":["Stressed","Overwhelmed"],"associations":["Work"],"note":"sounds wiped","confidence":0.88,"signals":["exhausted"]}`)
			return nil
		},

		Steps: []sim.Step{{
			Name: "run mood agent on a strongly-affect turn",
			Do: func(h *sim.Harness) error {
				res := runMood(h, mood.AgentConfig{}, 0, []mood.Turn{{
					Role:            "user",
					ScrubbedContent: "honestly I am absolutely exhausted today",
				}})
				if res.Action != mood.ActionAutoLogged {
					return fmt.Errorf("mood agent action = %q, reason: %s", res.Action, res.Reason)
				}
				return nil
			},
		}},

		Assertions: []sim.Assertion{
			{
				Name: "one mood entry is saved with source=inferred",
				Check: func(h *sim.Harness) error {
					entries, err := h.Store.RecentMoodEntries("", 5)
					if err != nil {
						return err
					}
					if len(entries) != 1 {
						return fmt.Errorf("mood entry count = %d, want 1", len(entries))
					}
					if entries[0].Source != memory.MoodSourceInferred {
						return fmt.Errorf("Source = %q, want inferred", entries[0].Source)
					}
					return nil
				},
			},
			{
				Name: "no Telegram message was sent",
				Check: func(h *sim.Harness) error {
					sends := h.Transport.MessagesByKind(sim.EventSend)
					if len(sends) != 0 {
						return fmt.Errorf("expected 0 sends on auto-log, got %d", len(sends))
					}
					return nil
				},
			},
			{
				Name: "no pending proposal row exists",
				Check: func(h *sim.Harness) error {
					due, err := h.Store.DuePendingMoodProposals(h.Clock.Now())
					if err != nil {
						return err
					}
					if len(due) != 0 {
						return fmt.Errorf("pending proposals = %d, want 0", len(due))
					}
					return nil
				},
			},
		},
	})
}

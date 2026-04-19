package scenarios

import (
	"fmt"

	"her/memory"
	"her/mood"
	"her/sim"
)

// mood_inferred_medium_proposal: the LLM returns medium confidence
// (0.4-0.75). Agent sends a Telegram message with three inline
// buttons (Log / Edit / No) and stashes a pending_mood_proposals row
// so a later tap can be resolved. Nothing is written to
// mood_entries yet — "confirmed" requires the user to actually tap.

func init() {
	sim.RegisterScenario(sim.Scenario{
		Name:        "mood_inferred_medium_proposal",
		Description: "Medium-confidence inferred mood emits a Telegram proposal with inline buttons and a pending_mood_proposals row.",

		Setup: func(h *sim.Harness) error {
			h.LLM.Script("", `{"skip":false,"valence":3,"labels":["Disappointed"],"associations":[],"note":"tone suggests letdown","confidence":0.55,"signals":[]}`)
			return nil
		},

		Steps: []sim.Step{{
			Name: "run mood agent on a neutral-ish turn",
			Do: func(h *sim.Harness) error {
				// No first-person affect words → heuristic stays
				// low → hybrid confidence matches the LLM's 0.55.
				res := runMood(h, mood.AgentConfig{}, 0, []mood.Turn{{
					Role:            "user",
					ScrubbedContent: "the review came back with a bunch of notes",
				}})
				if res.Action != mood.ActionProposalEmitted {
					return fmt.Errorf("mood agent action = %q, reason: %s", res.Action, res.Reason)
				}
				return nil
			},
		}},

		Assertions: []sim.Assertion{
			{
				Name: "exactly one Telegram message was sent",
				Check: func(h *sim.Harness) error {
					sends := h.Transport.MessagesByKind(sim.EventSend)
					if len(sends) != 1 {
						return fmt.Errorf("sends = %d, want 1", len(sends))
					}
					return nil
				},
			},
			{
				Name: "the proposal message carries 3 inline buttons",
				Check: func(h *sim.Harness) error {
					last := h.Transport.LastMessage()
					if last == nil {
						return fmt.Errorf("no last message")
					}
					if len(last.Buttons) != 3 {
						return fmt.Errorf("button count = %d, want 3", len(last.Buttons))
					}
					for _, want := range []string{"✅ Log it", "✏️ Edit", "❌ No"} {
						if btn := h.Transport.ButtonByLabel(last.MessageID, want); btn == nil {
							return fmt.Errorf("missing button %q", want)
						}
					}
					return nil
				},
			},
			{
				Name: "pending_mood_proposals has exactly one pending row",
				Check: func(h *sim.Harness) error {
					// DuePendingMoodProposals only returns rows past
					// expires_at; we expect zero due right now.
					due, _ := h.Store.DuePendingMoodProposals(h.Clock.Now())
					if len(due) != 0 {
						return fmt.Errorf("got %d due proposals right now, want 0 (should be in the future)", len(due))
					}
					// Direct lookup: find the one by message id.
					last := h.Transport.LastMessage()
					p, err := h.Store.PendingMoodProposalByMessageID(h.ChatID, int64(last.MessageID))
					if err != nil {
						return err
					}
					if p == nil {
						return fmt.Errorf("no pending proposal for msg %d", last.MessageID)
					}
					if p.Status != memory.MoodProposalPending {
						return fmt.Errorf("status = %q, want pending", p.Status)
					}
					return nil
				},
			},
			{
				Name: "no mood_entries row is written yet",
				Check: func(h *sim.Harness) error {
					entries, _ := h.Store.RecentMoodEntries("", 5)
					if len(entries) != 0 {
						return fmt.Errorf("got %d entries, want 0 (confirmed requires tap)", len(entries))
					}
					return nil
				},
			},
		},
	})
}

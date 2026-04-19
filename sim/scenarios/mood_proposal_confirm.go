package scenarios

import (
	"fmt"

	"her/memory"
	"her/mood"
	"her/sim"
)

// mood_proposal_confirm: user taps "Log it" on a medium-confidence
// proposal. The confirm handler saves the proposed entry with
// source=confirmed, flips the pending row's status, and edits the
// Telegram message to show success. No new LLM call needed — the
// proposal JSON already carries everything.

func init() {
	sim.RegisterScenario(sim.Scenario{
		Name:        "mood_proposal_confirm",
		Description: "Tapping 'Log it' on a proposal saves source=confirmed and flips status.",

		Setup: func(h *sim.Harness) error {
			h.LLM.Script("", `{"skip":false,"valence":3,"labels":["Disappointed"],"associations":[],"note":"letdown","confidence":0.55,"signals":[]}`)
			return nil
		},

		Steps: []sim.Step{
			{
				Name: "agent emits a medium-confidence proposal",
				Do: func(h *sim.Harness) error {
					res := runMood(h, mood.AgentConfig{}, 0, []mood.Turn{{
						Role:            "user",
						ScrubbedContent: "the review bounced it back again",
					}})
					if res.Action != mood.ActionProposalEmitted {
						return fmt.Errorf("proposal not emitted: %q", res.Action)
					}
					return nil
				},
			},
			{
				Name: "user taps 'Log it' — confirm handler runs",
				Do: func(h *sim.Harness) error {
					last := h.Transport.LastMessage()
					if last == nil {
						return fmt.Errorf("no proposal message present")
					}
					// Record the tap in the transport's event log
					// for symmetry with real Telegram callbacks.
					h.Transport.Dispatch(h.ChatID, last.MessageID, "mood_proposal", "confirm")

					editFn := func(chatID int64, msgID int, text string) error {
						return h.Transport.Edit(chatID, msgID, text)
					}
					_, err := mood.ConfirmProposal(h.Store, h.ChatID, int64(last.MessageID), editFn)
					return err
				},
			},
		},

		Assertions: []sim.Assertion{
			{
				Name: "one mood entry is saved with source=confirmed",
				Check: func(h *sim.Harness) error {
					entries, _ := h.Store.RecentMoodEntries("", 5)
					if len(entries) != 1 {
						return fmt.Errorf("entries = %d, want 1", len(entries))
					}
					if entries[0].Source != memory.MoodSourceConfirmed {
						return fmt.Errorf("Source = %q, want confirmed", entries[0].Source)
					}
					if entries[0].Valence != 3 {
						return fmt.Errorf("Valence = %d, want 3 (inherited from proposal)", entries[0].Valence)
					}
					return nil
				},
			},
			{
				Name: "pending proposal status flipped to confirmed",
				Check: func(h *sim.Harness) error {
					last := h.Transport.LastMessage()
					p, _ := h.Store.PendingMoodProposalByMessageID(h.ChatID, int64(last.MessageID))
					if p == nil {
						return fmt.Errorf("proposal row missing")
					}
					if p.Status != memory.MoodProposalConfirmed {
						return fmt.Errorf("status = %q, want confirmed", p.Status)
					}
					return nil
				},
			},
			{
				Name: "Telegram message was edited with a confirmation notice",
				Check: func(h *sim.Harness) error {
					edits := h.Transport.MessagesByKind(sim.EventEdit)
					if len(edits) != 1 {
						return fmt.Errorf("edit count = %d, want 1", len(edits))
					}
					if !contains(edits[0].Text, "logged") {
						return fmt.Errorf("edit text = %q, want substring 'logged'", edits[0].Text)
					}
					return nil
				},
			},
		},
	})
}

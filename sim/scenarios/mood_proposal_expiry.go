package scenarios

import (
	"fmt"
	"time"

	"her/memory"
	"her/mood"
	"her/sim"
)

// mood_proposal_expiry: a medium-confidence proposal sits untapped
// past its expiry. The sweeper edits the Telegram message in place,
// replacing the inline keyboard with an "expired" note, and flips
// the pending row's status to expired.

func init() {
	sim.RegisterScenario(sim.Scenario{
		Name:        "mood_proposal_expiry",
		Description: "Untapped proposal is edited to 'expired' and status flips after its expiry passes.",

		Setup: func(h *sim.Harness) error {
			h.LLM.Script("", `{"skip":false,"valence":3,"labels":["Disappointed"],"associations":[],"note":"letdown","confidence":0.55,"signals":[]}`)
			return nil
		},

		Steps: []sim.Step{
			{
				Name: "agent emits a medium-confidence proposal",
				Do: func(h *sim.Harness) error {
					res := runMood(h, mood.AgentConfig{ProposalExpiry: 30 * time.Minute}, 0, []mood.Turn{{
						Role:            "user",
						ScrubbedContent: "the review bounced it back for the third time",
					}})
					if res.Action != mood.ActionProposalEmitted {
						return fmt.Errorf("proposal not emitted: %q (reason: %s)",
							res.Action, res.Reason)
					}
					return nil
				},
			},
			{
				Name: "time jumps forward past the proposal's expiry",
				Do: func(h *sim.Harness) error {
					// 31 minutes — just past the 30-min default.
					h.Clock.Advance(31 * time.Minute)
					return nil
				},
			},
			{
				Name: "sweeper runs a single sweep",
				Do: func(h *sim.Harness) error {
					s := &mood.ProposalSweeper{
						Store: h.Store,
						Clock: h.Clock.Now,
						Edit: func(chatID int64, msgID int, text string) error {
							// Reuse the FakeTransport's Edit path so
							// the event shows up in the transcript.
							return h.Transport.Edit(chatID, msgID, text)
						},
					}
					s.Sweep(h.Ctx)
					return nil
				},
			},
		},

		Assertions: []sim.Assertion{
			{
				Name: "the Telegram message was edited to an expired notice",
				Check: func(h *sim.Harness) error {
					last := h.Transport.LastMessage()
					if last == nil {
						return fmt.Errorf("no last message")
					}
					if last.Kind != sim.EventEdit {
						return fmt.Errorf("last event kind = %q, want edit", last.Kind)
					}
					if !contains(last.Text, "expired") {
						return fmt.Errorf("edited text = %q, want substring 'expired'", last.Text)
					}
					return nil
				},
			},
			{
				Name: "pending_mood_proposals row status is 'expired'",
				Check: func(h *sim.Harness) error {
					last := h.Transport.LastMessage()
					p, err := h.Store.PendingMoodProposalByMessageID(h.ChatID, int64(last.MessageID))
					if err != nil {
						return err
					}
					if p == nil {
						return fmt.Errorf("proposal row missing after sweep")
					}
					if p.Status != memory.MoodProposalExpired {
						return fmt.Errorf("status = %q, want %q", p.Status, memory.MoodProposalExpired)
					}
					return nil
				},
			},
			{
				Name: "no mood_entries row was written (nothing to save on an untapped proposal)",
				Check: func(h *sim.Harness) error {
					entries, _ := h.Store.RecentMoodEntries("", 5)
					if len(entries) != 0 {
						return fmt.Errorf("entries = %d, want 0", len(entries))
					}
					return nil
				},
			},
		},
	})
}

// contains is a tiny helper used by a few scenarios — avoids the
// boilerplate of "import strings; strings.Contains" in every file.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

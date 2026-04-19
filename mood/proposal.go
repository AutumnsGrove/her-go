package mood

import (
	"encoding/json"
	"fmt"

	"her/memory"
)

// ConfirmProposal resolves a pending mood proposal the user tapped
// "Log it" on. It:
//
//   1. Looks up the pending row by Telegram message id.
//   2. Decodes the stored proposal JSON into a MoodEntry.
//   3. Saves the entry with source=confirmed.
//   4. Flips the proposal's status to confirmed.
//   5. Edits the original Telegram message to show it was logged.
//
// The Edit callback is optional — pass nil when you don't need the
// message updated (e.g. tests that only care about the DB). Any
// error at the DB layer rolls back nothing; partial success is OK
// here because the proposal status flip is a best-effort audit
// trail, not a correctness guarantee.
func ConfirmProposal(
	store *memory.Store,
	chatID, messageID int64,
	edit func(chatID int64, messageID int, text string) error,
) (*memory.MoodEntry, error) {
	p, err := store.PendingMoodProposalByMessageID(chatID, messageID)
	if err != nil {
		return nil, fmt.Errorf("ConfirmProposal: lookup: %w", err)
	}
	if p == nil {
		return nil, fmt.Errorf("ConfirmProposal: no pending proposal for msg %d", messageID)
	}
	if p.Status != memory.MoodProposalPending {
		return nil, fmt.Errorf("ConfirmProposal: proposal status = %q, want pending", p.Status)
	}

	var entry memory.MoodEntry
	if err := json.Unmarshal(p.ProposalJSON, &entry); err != nil {
		return nil, fmt.Errorf("ConfirmProposal: decode proposal: %w", err)
	}

	// The LLM's original confidence is preserved, but the source is
	// upgraded to confirmed because the USER explicitly agreed.
	entry.Source = memory.MoodSourceConfirmed
	// Reset ID so SaveMoodEntry allocates a fresh one.
	entry.ID = 0

	id, err := store.SaveMoodEntry(&entry)
	if err != nil {
		return nil, fmt.Errorf("ConfirmProposal: save entry: %w", err)
	}
	entry.ID = id

	if err := store.UpdatePendingMoodProposalStatus(p.ID, memory.MoodProposalConfirmed); err != nil {
		log.Warn("ConfirmProposal: status update failed", "proposal_id", p.ID, "err", err)
	}

	if edit != nil {
		text := "✅ mood logged."
		if err := edit(chatID, int(messageID), text); err != nil {
			log.Warn("ConfirmProposal: edit failed", "msg_id", messageID, "err", err)
		}
	}

	return &entry, nil
}

// RejectProposal flips a pending proposal to rejected and edits the
// Telegram message to reflect that the user said no. Nothing is
// written to mood_entries — the whole point is the user declined
// the inference.
func RejectProposal(
	store *memory.Store,
	chatID, messageID int64,
	edit func(chatID int64, messageID int, text string) error,
) error {
	p, err := store.PendingMoodProposalByMessageID(chatID, messageID)
	if err != nil {
		return fmt.Errorf("RejectProposal: lookup: %w", err)
	}
	if p == nil {
		return fmt.Errorf("RejectProposal: no pending proposal for msg %d", messageID)
	}
	if err := store.UpdatePendingMoodProposalStatus(p.ID, memory.MoodProposalRejected); err != nil {
		return fmt.Errorf("RejectProposal: status update: %w", err)
	}
	if edit != nil {
		_ = edit(chatID, int(messageID), "❌ skipped — no mood logged.")
	}
	return nil
}

package mood

import (
	"context"
	"time"

	"her/memory"
)

// ProposalSweeper periodically expires pending mood proposals whose
// expires_at has passed. When a proposal expires, the sweeper edits
// the original Telegram message in place — replacing the inline
// keyboard with a short "expired" note — and flips the row's status
// to expired.
//
// Runs as a long-lived goroutine owned by the bot. One sweeper per
// bot; it queries across all chats by scanning the
// pending_mood_proposals table directly.
type ProposalSweeper struct {
	Store memory.Store

	// Edit rewrites a Telegram message in place. Matches the
	// FakeTransport.Edit signature in the sim so scenarios can
	// verify expiry UX without a real Telegram.
	Edit func(chatID int64, messageID int, newText string) error

	// Clock lets sim scenarios advance time. Default: time.Now.
	Clock func() time.Time

	// Interval is how often the sweeper runs. Default 5m — short
	// enough that expired proposals don't linger visibly but long
	// enough that SQLite isn't hammered.
	Interval time.Duration
}

// Run starts the sweeper loop. Blocks until ctx is cancelled.
// Typically called inside a `go` statement at bot startup.
func (s *ProposalSweeper) Run(ctx context.Context) {
	interval := s.Interval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	if s.Clock == nil {
		s.Clock = time.Now
	}

	// Sweep once immediately so stale proposals from before the
	// bot started don't wait the full interval.
	s.Sweep(ctx)

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Sweep(ctx)
		}
	}
}

// Sweep runs one pass: find due proposals, edit their Telegram
// messages, mark them expired. Exported so tests and sim scenarios
// can drive the sweep synchronously.
func (s *ProposalSweeper) Sweep(_ context.Context) {
	due, err := s.Store.DuePendingMoodProposals(s.Clock())
	if err != nil {
		log.Warn("proposal sweeper: fetching due", "err", err)
		return
	}
	for _, p := range due {
		// Expiry message keeps the user's attention off the proposal
		// without forcing a hard "you ignored it" tone. The "revisit"
		// hint points at /mood recent which the wizard exposes.
		text := "⏳ mood check — expired. Tap /mood recent to revisit."
		if s.Edit != nil {
			if err := s.Edit(p.TelegramChatID, int(p.TelegramMessageID), text); err != nil {
				log.Warn("proposal sweeper: edit failed",
					"proposal_id", p.ID, "err", err)
				// Still flip status — if we can't edit the message
				// (e.g. user deleted it), leaving the row "pending"
				// forever would re-trigger every sweep.
			}
		}
		if err := s.Store.UpdatePendingMoodProposalStatus(p.ID, memory.MoodProposalExpired); err != nil {
			log.Warn("proposal sweeper: status update",
				"proposal_id", p.ID, "err", err)
		}
	}
	if len(due) > 0 {
		log.Info("proposal sweeper", "expired", len(due))
	}
}

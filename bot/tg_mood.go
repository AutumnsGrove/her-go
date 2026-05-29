// bot/tg_mood.go — Telegram-specific mood UI: proposals, sweeper,
// graph rendering, and inline button callbacks.
//
// General mood agent wiring (initMood, launchMoodAgent) lives in mood.go.
package bot

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"her/memory"
	"her/mood"

	tele "gopkg.in/telebot.v4"
)

// initMoodTelegram wires the Telegram-specific parts of the mood
// pipeline: proposal sweeper and inline button handlers. Called from
// initMood() — no-op when b.tb is nil (dev/gateway mode).
func (b *Bot) initMoodTelegram() {
	if b.tb == nil {
		return
	}

	sweeperInterval := time.Duration(b.cfg.Mood.SweeperIntervalMinutes) * time.Minute
	if sweeperInterval == 0 {
		sweeperInterval = 5 * time.Minute
	}
	b.moodSweeper = &mood.ProposalSweeper{
		Store:    b.store,
		Edit:     b.editTelegramMessage,
		Interval: sweeperInterval,
	}

	b.tb.Handle(&tele.InlineButton{Unique: "mood_proposal"}, b.handleMoodProposalCallback)
	b.tb.Handle(&tele.InlineButton{Unique: "mood_wizard"}, b.handleMoodWizardCallback)
}

// sendMoodProposal is the Propose callback the mood agent invokes for
// medium-confidence inferences. It sends a Telegram message with three
// inline buttons (Log it / Edit / No) and returns the chat + message
// IDs so the agent can persist the pending proposal row keyed on them.
func (b *Bot) sendMoodProposal(
	_ context.Context,
	entry *memory.MoodEntry,
	_ time.Time,
) (int64, int64, error) {
	if b.ownerChat == 0 {
		return 0, 0, fmt.Errorf("sendMoodProposal: ownerChat is zero")
	}

	tier := "unpleasant"
	if entry.Valence == 4 {
		tier = "neutral"
	} else if entry.Valence >= 5 {
		tier = "pleasant"
	}
	labelLine := ""
	if len(entry.Labels) > 0 {
		labelLine = " — " + strings.Join(entry.Labels, ", ")
	}
	text := fmt.Sprintf("reading this as %s%s. log it?", tier, labelLine)

	markup := &tele.ReplyMarkup{}
	row := markup.Row(
		markup.Data("✅ log it", "mood_proposal", "confirm"),
		markup.Data("✏️ edit", "mood_proposal", "edit"),
		markup.Data("❌ no", "mood_proposal", "reject"),
	)
	markup.Inline(row)

	chat := &tele.Chat{ID: b.ownerChat}
	msg, err := b.tb.Send(chat, text, &tele.SendOptions{ReplyMarkup: markup})
	if err != nil {
		return 0, 0, fmt.Errorf("send proposal: %w", err)
	}
	return b.ownerChat, int64(msg.ID), nil
}

// editTelegramMessage is the Edit callback the sweeper uses when a
// proposal expires. Matches the shape mood.ProposalSweeper expects.
func (b *Bot) editTelegramMessage(chatID int64, messageID int, text string) error {
	msg := &tele.Message{
		ID:   messageID,
		Chat: &tele.Chat{ID: chatID},
	}
	_, err := b.tb.Edit(msg, text)
	return err
}

// handleMoodProposalCallback routes taps on the mood proposal inline
// keyboard. The Data field carries the action: "confirm", "reject", or "edit".
func (b *Bot) handleMoodProposalCallback(c tele.Context) error {
	data := strings.TrimSpace(c.Callback().Data)
	chatID := c.Callback().Message.Chat.ID
	msgID := int64(c.Callback().Message.ID)

	switch data {
	case "confirm":
		entry, err := mood.ConfirmProposal(b.store, chatID, msgID, b.editTelegramMessage)
		if err != nil {
			log.Error("mood proposal confirm", "err", err)
			_ = c.Respond(&tele.CallbackResponse{Text: "couldn't save — try again?"})
			return nil
		}
		log.Info("mood proposal confirmed", "entry_id", entry.ID)
		_ = c.Respond(&tele.CallbackResponse{Text: "logged"})
		return nil

	case "reject":
		if err := mood.RejectProposal(b.store, chatID, msgID, b.editTelegramMessage); err != nil {
			log.Error("mood proposal reject", "err", err)
			_ = c.Respond(&tele.CallbackResponse{Text: "something went wrong"})
			return nil
		}
		_ = c.Respond(&tele.CallbackResponse{Text: "skipped"})
		return nil

	case "edit":
		_ = mood.RejectProposal(b.store, chatID, msgID, b.editTelegramMessage)
		_ = c.Respond(&tele.CallbackResponse{Text: "manual edit is coming soon — dropped this one for now"})
		return nil

	default:
		_ = c.Respond(&tele.CallbackResponse{Text: "unknown option"})
		return nil
	}
}

// Thin aliases so the wizard command handler (in bot/mood_wizard.go)
// doesn't have to import mood directly.
const (
	moodGraphRangeWeek  = mood.GraphRangeWeek
	moodGraphRangeMonth = mood.GraphRangeMonth
	moodGraphRangeYear  = mood.GraphRangeYear
)

// sendMoodGraph renders a PNG of the user's mood trajectory for the
// given range and sends it as a Telegram photo reply.
func (b *Bot) sendMoodGraph(c tele.Context, r mood.GraphRange) error {
	png, err := mood.RenderValencePNG(b.store, b.moodVocab, r, time.Now())
	if err != nil {
		log.Error("mood graph render", "range", r, "err", err)
		return c.Send("couldn't render the mood chart right now.")
	}

	photo := &tele.Photo{
		File:    tele.FromReader(bytes.NewReader(png)),
		Caption: fmt.Sprintf("📈 mood — last %s", r.String()),
	}
	return c.Send(photo)
}

// startMoodSweeper launches the proposal-expiry sweeper goroutine.
// The sweeper shuts down when Bot.Stop() calls the stored cancel func.
func (b *Bot) startMoodSweeper() {
	if b.moodSweeper == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	b.moodSweeperStop = cancel
	go b.moodSweeper.Run(ctx)
	log.Info("mood proposal sweeper started", "interval", b.moodSweeper.Interval)
}

// parseMoodProposalData is a small helper for tests and CLI paths that
// need to reason about callback payloads without invoking telebot.
func parseMoodProposalData(data string) (action string) {
	s := strings.TrimSpace(data)
	if s == "" {
		return "unknown"
	}
	return s
}

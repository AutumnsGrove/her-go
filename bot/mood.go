package bot

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"her/memory"
	"her/mood"

	tele "gopkg.in/telebot.v4"
)

// initMood wires the mood runner + proposal sweeper onto the Bot
// struct. Called from New() when cfg.MoodAgent.Model is non-empty.
// The sweeper goroutine is launched later by Start() so it shares
// the bot's lifecycle.
func (b *Bot) initMood() error {
	vocab := mood.Default()
	if b.cfg.Mood.VocabPath != "" {
		v, err := mood.LoadVocab(b.cfg.Mood.VocabPath)
		if err != nil {
			return fmt.Errorf("loading mood vocab: %w", err)
		}
		vocab = v
	}
	b.moodVocab = vocab

	// Fill in AgentConfig defaults from config.yaml.
	high := b.cfg.Mood.ConfidenceHigh
	if high == 0 {
		high = 0.75
	}
	low := b.cfg.Mood.ConfidenceLow
	if low == 0 {
		low = 0.40
	}
	dedupWin := time.Duration(b.cfg.Mood.DedupWindowMinutes) * time.Minute
	if dedupWin == 0 {
		dedupWin = 2 * time.Hour
	}
	dedupSim := b.cfg.Mood.DedupSimilarity
	if dedupSim == 0 {
		dedupSim = 0.80
	}
	proposalExpiry := time.Duration(b.cfg.Mood.ProposalExpiryMinutes) * time.Minute
	if proposalExpiry == 0 {
		proposalExpiry = 30 * time.Minute
	}
	ctxTurns := b.cfg.Mood.ContextTurns
	if ctxTurns == 0 {
		ctxTurns = 5
	}

	// Embed bridge: mood.Deps expects a context-aware signature;
	// embed.Client.Embed only takes text.
	embedFn := func(_ context.Context, text string) ([]float32, error) {
		if b.embedClient == nil {
			return nil, nil
		}
		return b.embedClient.Embed(text)
	}

	b.moodRunner = &mood.Runner{
		Deps: mood.Deps{
			LLM:        b.moodAgentLLM,
			Classifier: b.classifierLLM, // reuse main classifier
			Store:      b.store,
			Vocab:      vocab,
			Embed:      embedFn,
			Propose:    b.sendMoodProposal,
		},
		Config: mood.AgentConfig{
			ContextTurns:    ctxTurns,
			ConfidenceHigh:  high,
			ConfidenceLow:   low,
			DedupWindow:     dedupWin,
			DedupSimilarity: dedupSim,
			ProposalExpiry:  proposalExpiry,
		},
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

	// Register the mood_proposal callback handler (Log it / Edit / No).
	b.tb.Handle(&tele.InlineButton{Unique: "mood_proposal"}, b.handleMoodProposalCallback)

	// Register the /mood wizard callback handler + command.
	b.tb.Handle(&tele.InlineButton{Unique: "mood_wizard"}, b.handleMoodWizardCallback)

	return nil
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
	// Need a concrete chat to target. In a personal-use bot this is
	// the owner chat; if unset, there's nowhere to send it.
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
// keyboard. The Data field (set via markup.Data's third arg) carries
// the action name — "confirm", "reject", or "edit".
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
		// Edit routes to /mood <existing proposal id> wizard — that
		// lands with the dedicated wizard work. Until then, mark the
		// proposal as rejected so it doesn't haunt the expiry sweep,
		// and tell the user we're not quite there yet.
		_ = mood.RejectProposal(b.store, chatID, msgID, b.editTelegramMessage)
		_ = c.Respond(&tele.CallbackResponse{Text: "manual edit is coming soon — dropped this one for now"})
		return nil

	default:
		_ = c.Respond(&tele.CallbackResponse{Text: "unknown option"})
		return nil
	}
}

// Thin aliases so the wizard command handler (in bot/mood_wizard.go)
// doesn't have to import mood directly. Keeping the coupling narrow.
const (
	moodGraphRangeWeek  = mood.GraphRangeWeek
	moodGraphRangeMonth = mood.GraphRangeMonth
	moodGraphRangeYear  = mood.GraphRangeYear
)

// sendMoodGraph renders a PNG of the user's mood trajectory for the
// given range and sends it as a Telegram photo reply. Runs
// synchronously — the render is sub-second for a month of data.
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

// launchMoodAgent fires mood.Runner.RunForConversation in a goroutine.
// Called from runAgent after the main reply is sent. No-op when the
// mood runner isn't configured. When trace is non-nil, decision-point
// traces flow into its slot of the turn's TraceBoard.
func (b *Bot) launchMoodAgent(convID string, trace func(string) error) {
	if b.moodRunner == nil || convID == "" {
		return
	}
	go func() {
		// 60s timeout — mood agent does one LLM call plus an
		// optional classifier pass. Safely past any reasonable
		// round-trip.
		var res mood.Result
		if trace != nil {
			res = b.moodRunner.RunForConversationWithTrace(
				context.Background(), convID, 60*time.Second, trace,
			)
		} else {
			res = b.moodRunner.RunForConversationWithTimeout(
				context.Background(), convID, 60*time.Second,
			)
		}
		if res.Action == mood.ActionErrored {
			log.Warn("mood agent errored", "reason", res.Reason)
		}
	}()
}

// startMoodSweeper launches the proposal-expiry sweeper goroutine. The
// sweeper shuts down when the returned cancel fn is called; Start()
// drops the cancel into the bot's lifecycle at some future point. For
// now it's fire-and-forget and the OS reclaims it on shutdown.
func (b *Bot) startMoodSweeper() {
	if b.moodSweeper == nil {
		return
	}
	go b.moodSweeper.Run(context.Background())
	log.Info("mood proposal sweeper started", "interval", b.moodSweeper.Interval)
}

// parseMoodProposalData is a small helper for tests and CLI paths that
// need to reason about callback payloads without invoking telebot.
// Kept exported so the sim (when it grows) can use it.
func parseMoodProposalData(data string) (action string) {
	s := strings.TrimSpace(data)
	if s == "" {
		return "unknown"
	}
	// Data is one of: confirm | reject | edit (set above).
	return s
}

// formatProposalForLog is used by the log line we emit when firing a
// proposal. Kept private; exposed via its callers. strconv import
// is used here so Go doesn't complain in future additions.
var _ = strconv.Itoa

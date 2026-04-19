package bot

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"her/memory"

	tele "gopkg.in/telebot.v4"
)

// wizardState tracks an in-flight /mood session. One per chat id, kept
// in Bot.moodWizards (sync.Map). State persists across button taps so
// the user can build up their mood entry incrementally.
//
// Steps:
//
//	1 — valence picker (7 buckets)
//	2 — label multi-select filtered by tier
//	3 — association multi-select
//	4 — note prompt (text reply or skip)
//	0 — fresh / completed / cancelled (absent from the map)
type wizardState struct {
	mu sync.Mutex

	Step         int
	Valence      int
	Labels       []string
	Associations []string
	Note         string

	// MessageID is the single message the wizard edits in place.
	// Telegram "edit message" keeps the chat tidy; the inline keyboard
	// swaps in and out as the user advances steps.
	MessageID int
	ChatID    int64

	ExpiresAt time.Time
}

// wizardTTL is how long an idle wizard stays in the map before the
// sweeper GCs it. 10 minutes matches the plan.
const wizardTTL = 10 * time.Minute

// handleMoodCommand routes /mood. With no argument it starts the
// wizard; with "week" / "month" / "year" it sends a PNG graph.
func (b *Bot) handleMoodCommand(c tele.Context) error {
	if b.moodVocab == nil {
		return c.Send("mood tracking isn't configured on this bot.")
	}

	arg := strings.ToLower(strings.TrimSpace(c.Message().Payload))
	switch arg {
	case "":
		return b.startMoodWizard(c)
	case "week":
		return b.sendMoodGraph(c, moodGraphRangeWeek)
	case "month":
		return b.sendMoodGraph(c, moodGraphRangeMonth)
	case "year":
		return b.sendMoodGraph(c, moodGraphRangeYear)
	default:
		return c.Send("usage: /mood | /mood week | /mood month | /mood year")
	}
}

// startMoodWizard kicks off the multi-step picker. Replaces any
// in-flight wizard for the chat (second /mood wins).
func (b *Bot) startMoodWizard(c tele.Context) error {
	chatID := c.Message().Chat.ID
	w := &wizardState{
		Step:      1,
		ChatID:    chatID,
		ExpiresAt: time.Now().Add(wizardTTL),
	}

	markup := b.moodValenceKeyboard()
	msg, err := c.Bot().Send(&tele.Chat{ID: chatID},
		"how are you feeling right now?",
		&tele.SendOptions{ReplyMarkup: markup},
	)
	if err != nil {
		return err
	}
	w.MessageID = msg.ID

	b.moodWizards.Store(chatID, w)
	go b.expireWizardAfter(chatID, w, wizardTTL)
	return nil
}

// handleMoodWizardCallback routes inline button taps through the
// wizard state machine. Callback data encodes the user's action —
// "valence:3", "label:Sad", "assoc:Work", "next", "skip", "save",
// "cancel".
func (b *Bot) handleMoodWizardCallback(c tele.Context) error {
	chatID := c.Callback().Message.Chat.ID
	raw, ok := b.moodWizards.Load(chatID)
	if !ok {
		_ = c.Respond(&tele.CallbackResponse{Text: "this mood session expired — try /mood again"})
		_ = c.Edit("⏳ mood session expired.")
		return nil
	}
	w := raw.(*wizardState)
	w.mu.Lock()
	defer w.mu.Unlock()

	data := strings.TrimSpace(c.Callback().Data)
	action, arg := parseWizardData(data)

	// Silent ack so Telegram doesn't show the spinner.
	_ = c.Respond(&tele.CallbackResponse{})

	switch action {
	case "cancel":
		b.moodWizards.Delete(chatID)
		return c.Edit("🫥 mood entry cancelled.")

	case "valence":
		v, err := atoiSafe(arg)
		if err != nil || v < 1 || v > 7 {
			return nil
		}
		w.Valence = v
		w.Step = 2
		return c.Edit(b.wizardLabelsPrompt(w), b.moodLabelsKeyboard(w))

	case "label":
		if w.Step != 2 {
			return nil
		}
		w.Labels = toggle(w.Labels, arg)
		return c.Edit(b.wizardLabelsPrompt(w), b.moodLabelsKeyboard(w))

	case "assoc":
		if w.Step != 3 {
			return nil
		}
		w.Associations = toggle(w.Associations, arg)
		return c.Edit(b.wizardAssocsPrompt(w), b.moodAssocsKeyboard(w))

	case "next":
		// Advance: step 2 → 3 → 4.
		switch w.Step {
		case 2:
			if len(w.Labels) == 0 {
				_ = c.Respond(&tele.CallbackResponse{Text: "pick at least one label, or tap cancel"})
				return nil
			}
			w.Step = 3
			return c.Edit(b.wizardAssocsPrompt(w), b.moodAssocsKeyboard(w))
		case 3:
			w.Step = 4
			return c.Edit(b.wizardNotePrompt(w), b.moodNoteKeyboard())
		}
		return nil

	case "skip":
		// "skip" on the note step = save without a note.
		if w.Step == 4 {
			return b.finalizeWizard(c, w, "")
		}
		// "skip" on associations = advance without selecting any.
		if w.Step == 3 {
			w.Associations = nil
			w.Step = 4
			return c.Edit(b.wizardNotePrompt(w), b.moodNoteKeyboard())
		}
		return nil

	case "save":
		if w.Step == 4 {
			return b.finalizeWizard(c, w, w.Note)
		}
		return nil
	}

	return nil
}

// HandleMoodWizardNote is called by the bot's text-message handler
// when a wizard is waiting on step 4. Intercepts the text as the
// note, finalizes the wizard. Returns true when the message was
// consumed by the wizard — the caller should NOT route it through
// agent.Run.
func (b *Bot) HandleMoodWizardNote(c tele.Context) (consumed bool) {
	chatID := c.Message().Chat.ID
	raw, ok := b.moodWizards.Load(chatID)
	if !ok {
		return false
	}
	w := raw.(*wizardState)
	w.mu.Lock()
	step := w.Step
	w.mu.Unlock()
	if step != 4 {
		return false
	}

	note := strings.TrimSpace(c.Message().Text)
	if note == "" {
		return false
	}

	// Finalize.
	if err := b.finalizeWizard(c, w, note); err != nil {
		log.Warn("mood wizard finalize (note)", "err", err)
	}
	// Delete the user's message so the chat stays clean? telebot
	// supports c.Delete(), but that requires bot admin in groups.
	// In a private 1:1 chat it's fine — try and swallow errors.
	_ = c.Delete()
	return true
}

// finalizeWizard writes the mood_entries row with source=manual and
// cleans up the wizard state. The message is edited to a compact
// confirmation so the user sees what landed in the DB.
func (b *Bot) finalizeWizard(c tele.Context, w *wizardState, note string) error {
	entry := &memory.MoodEntry{
		Timestamp:    time.Now(),
		Kind:         memory.MoodKindMomentary,
		Valence:      w.Valence,
		Labels:       append([]string(nil), w.Labels...),
		Associations: append([]string(nil), w.Associations...),
		Note:         note,
		Source:       memory.MoodSourceManual,
		Confidence:   1.0, // manual entries aren't guesses
	}
	if _, err := b.store.SaveMoodEntry(entry); err != nil {
		_ = c.Edit("couldn't save mood entry: " + err.Error())
		return err
	}

	b.moodWizards.Delete(w.ChatID)
	confirm := fmt.Sprintf("✅ logged: valence %d/7", w.Valence)
	if len(w.Labels) > 0 {
		confirm += " — " + strings.Join(w.Labels, ", ")
	}
	if len(w.Associations) > 0 {
		confirm += " (" + strings.Join(w.Associations, ", ") + ")"
	}
	return c.Edit(confirm)
}

// expireWizardAfter sleeps for d, then removes the wizard if it's
// still present and the same instance. Safe to call many times per
// wizard; only one runs per wizard because we guard on equality.
func (b *Bot) expireWizardAfter(chatID int64, w *wizardState, d time.Duration) {
	time.Sleep(d)
	raw, ok := b.moodWizards.Load(chatID)
	if !ok || raw.(*wizardState) != w {
		return
	}
	b.moodWizards.Delete(chatID)

	// Best-effort edit to tell the user the session aged out.
	_ = b.editTelegramMessage(chatID, w.MessageID, "⏳ mood session expired.")
}

// --- Prompt text builders ------------------------------------------

func (b *Bot) wizardLabelsPrompt(w *wizardState) string {
	selected := "none yet"
	if len(w.Labels) > 0 {
		selected = strings.Join(w.Labels, ", ")
	}
	return fmt.Sprintf("valence set to %d/7.\n\nwhat feeling fits? (tap to toggle)\nselected: %s",
		w.Valence, selected)
}

func (b *Bot) wizardAssocsPrompt(w *wizardState) string {
	selected := "none"
	if len(w.Associations) > 0 {
		selected = strings.Join(w.Associations, ", ")
	}
	return fmt.Sprintf("what's contributing to this mood? (tap to toggle, or skip)\nselected: %s",
		selected)
}

func (b *Bot) wizardNotePrompt(w *wizardState) string {
	return fmt.Sprintf("valence %d/7 with labels [%s]%s\n\nwant to add a note? reply with text, or tap save/skip.",
		w.Valence,
		strings.Join(w.Labels, ", "),
		assocTail(w.Associations),
	)
}

func assocTail(assocs []string) string {
	if len(assocs) == 0 {
		return ""
	}
	return " re: " + strings.Join(assocs, ", ")
}

// --- Keyboard builders ---------------------------------------------

// moodValenceKeyboard is the step-1 picker — 7 valence buttons in
// two rows (4 + 3) + a cancel row.
func (b *Bot) moodValenceKeyboard() *tele.ReplyMarkup {
	m := &tele.ReplyMarkup{}
	row1 := []tele.Btn{}
	row2 := []tele.Btn{}
	for i := 1; i <= 7; i++ {
		bucket := b.moodVocab.Buckets[i]
		label := fmt.Sprintf("%s %d", bucket.Emoji, i)
		btn := m.Data(label, "mood_wizard", fmt.Sprintf("valence:%d", i))
		if i <= 4 {
			row1 = append(row1, btn)
		} else {
			row2 = append(row2, btn)
		}
	}
	m.Inline(
		m.Row(row1...),
		m.Row(row2...),
		m.Row(m.Data("cancel", "mood_wizard", "cancel")),
	)
	return m
}

// moodLabelsKeyboard renders the step-2 picker: labels for the
// chosen valence's tier, two per row, with a checkmark prefix on
// selected items. Ends with a row of "done" + "cancel".
func (b *Bot) moodLabelsKeyboard(w *wizardState) *tele.ReplyMarkup {
	labels := b.moodVocab.LabelsForValence(w.Valence)
	return b.moodChipKeyboard("label", labels, w.Labels, true)
}

// moodAssocsKeyboard renders the step-3 picker: associations with a
// skip button (since associations are optional).
func (b *Bot) moodAssocsKeyboard(w *wizardState) *tele.ReplyMarkup {
	assocs := b.moodVocab.Associations()
	return b.moodChipKeyboard("assoc", assocs, w.Associations, false)
}

// moodChipKeyboard is the shared chip-picker builder for labels and
// associations. requiresSelection=true means "Done" only works if at
// least one chip is picked; false means "skip" is always available.
func (b *Bot) moodChipKeyboard(prefix string, all, selected []string, requiresSelection bool) *tele.ReplyMarkup {
	m := &tele.ReplyMarkup{}
	selectedSet := map[string]bool{}
	for _, s := range selected {
		selectedSet[s] = true
	}

	rows := [][]tele.Btn{}
	var current []tele.Btn
	const chipsPerRow = 2
	for _, label := range all {
		display := label
		if selectedSet[label] {
			display = "✓ " + label
		}
		current = append(current, m.Data(display, "mood_wizard", prefix+":"+label))
		if len(current) >= chipsPerRow {
			rows = append(rows, current)
			current = nil
		}
	}
	if len(current) > 0 {
		rows = append(rows, current)
	}

	if requiresSelection {
		rows = append(rows, []tele.Btn{
			m.Data("done →", "mood_wizard", "next"),
			m.Data("cancel", "mood_wizard", "cancel"),
		})
	} else {
		rows = append(rows, []tele.Btn{
			m.Data("skip", "mood_wizard", "skip"),
			m.Data("done →", "mood_wizard", "next"),
			m.Data("cancel", "mood_wizard", "cancel"),
		})
	}

	out := make([]tele.Row, 0, len(rows))
	for _, r := range rows {
		out = append(out, m.Row(r...))
	}
	m.Inline(out...)
	return m
}

// moodNoteKeyboard is the step-4 footer — save or skip. The user can
// also reply with a text message, which the text handler catches.
func (b *Bot) moodNoteKeyboard() *tele.ReplyMarkup {
	m := &tele.ReplyMarkup{}
	m.Inline(m.Row(
		m.Data("save (no note)", "mood_wizard", "skip"),
		m.Data("save", "mood_wizard", "save"),
		m.Data("cancel", "mood_wizard", "cancel"),
	))
	return m
}

// --- Tiny helpers --------------------------------------------------

// parseWizardData splits "action:arg" into ("action", "arg"). When
// there's no colon, arg is empty.
func parseWizardData(data string) (action, arg string) {
	i := strings.IndexByte(data, ':')
	if i < 0 {
		return data, ""
	}
	return data[:i], data[i+1:]
}

// toggle flips the presence of s in the slice. Returns a new slice
// so callers don't have to worry about aliasing.
func toggle(in []string, s string) []string {
	out := make([]string, 0, len(in)+1)
	found := false
	for _, x := range in {
		if x == s {
			found = true
			continue
		}
		out = append(out, x)
	}
	if !found {
		out = append(out, s)
	}
	return out
}

// atoiSafe parses a small integer without importing strconv at every
// call site.
func atoiSafe(s string) (int, error) {
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a number")
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

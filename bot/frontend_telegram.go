package bot

import (
	"fmt"
	"sync"
	"time"

	tele "gopkg.in/telebot.v4"
)

// TelegramFrontend implements Frontend for the Telegram transport.
// It wraps a tele.Context and manages the placeholder message that
// gets edited with status updates and the final reply.
//
// The placeholder pattern is the key UX trick: we send a 💭 message
// immediately so the user sees the bot is working, then edit it in
// place as the agent progresses. This avoids the "typing..." indicator
// disappearing after 5 seconds.
type TelegramFrontend struct {
	c           tele.Context
	b           *Bot
	placeholder *tele.Message
	html        bool // whether placeholder was sent with HTML parse mode
}

// NewTelegramFrontend creates a frontend for a Telegram message context.
func NewTelegramFrontend(c tele.Context, b *Bot) *TelegramFrontend {
	return &TelegramFrontend{c: c, b: b}
}

func (f *TelegramFrontend) SendPlaceholder(text string, html bool) error {
	f.html = html
	var opts []interface{}
	if html {
		opts = append(opts, &tele.SendOptions{ParseMode: tele.ModeHTML})
	}
	msg, err := f.c.Bot().Send(f.c.Recipient(), text, opts...)
	if err != nil {
		return err
	}
	f.placeholder = msg
	return nil
}

func (f *TelegramFrontend) EditStatus(text string) error {
	if f.placeholder == nil {
		return nil
	}
	if f.html {
		_, err := f.c.Bot().Edit(f.placeholder, text, &tele.SendOptions{ParseMode: tele.ModeHTML})
		return err
	}
	_, err := f.c.Bot().Edit(f.placeholder, text)
	return err
}

func (f *TelegramFrontend) SendReply(text string) error {
	_, err := f.c.Bot().Send(f.c.Recipient(), text, &tele.SendOptions{ParseMode: tele.ModeHTML})
	return err
}

func (f *TelegramFrontend) SendPaginated(text string) error {
	return f.b.sendPaginated(f.c, text)
}

func (f *TelegramFrontend) SendConfirm(text string) (int64, error) {
	markup := &tele.ReplyMarkup{}
	btnYes := markup.Data("Yes", "confirm", "yes")
	btnNo := markup.Data("No", "confirm", "no")
	markup.Inline(markup.Row(btnYes, btnNo))

	msg, err := f.c.Bot().Send(f.c.Recipient(), text, &tele.SendOptions{
		ParseMode:   tele.ModeHTML,
		ReplyMarkup: markup,
	})
	if err != nil {
		return 0, err
	}
	return int64(msg.ID), nil
}

func (f *TelegramFrontend) StageReset() error {
	newPlaceholder, err := f.c.Bot().Send(f.c.Recipient(), "\U0001F4AD")
	if err != nil {
		return fmt.Errorf("stage reset: sending new placeholder: %w", err)
	}
	f.placeholder = newPlaceholder
	return nil
}

func (f *TelegramFrontend) DeletePlaceholder() error {
	if f.placeholder == nil {
		return nil
	}
	return f.c.Bot().Delete(f.placeholder)
}

// StartTyping launches the Telegram typing indicator, refreshed every
// 4 seconds (Telegram's indicator expires after ~5s). Returns a
// function that stops it — safe to call multiple times (wrapped in
// sync.Once so closing the channel can't panic).
func (f *TelegramFrontend) StartTyping() func() {
	stopTyping := make(chan struct{})
	go func() {
		_ = f.c.Notify(tele.Typing)
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopTyping:
				return
			case <-ticker.C:
				_ = f.c.Notify(tele.Typing)
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(stopTyping) }) }
}

func (f *TelegramFrontend) SupportsStreaming() bool { return f.b.cfg.Chat.Streaming }

func (f *TelegramFrontend) OnStreamToken(token string) {
	// Streaming is handled by makeStreamCallback which operates on
	// the placeholder directly. The frontend's placeholder is shared,
	// so the stream callback can edit it.
}

func (f *TelegramFrontend) StopStream() {}

func (f *TelegramFrontend) SendBusy() error {
	return f.c.Send("Still working on your last message — give me just a moment.")
}

func (f *TelegramFrontend) SendError(text string) error {
	if f.placeholder != nil {
		_, _ = f.c.Bot().Edit(f.placeholder, text)
		return nil
	}
	return f.c.Send(text)
}

func (f *TelegramFrontend) ReplyText() string { return "" }

// Placeholder returns the current placeholder message. Used by the
// stream callback builder which needs direct access to the message
// for incremental edits.
func (f *TelegramFrontend) Placeholder() *tele.Message { return f.placeholder }

// Context returns the underlying tele.Context for Telegram-specific
// operations (TTS, voice replies) that don't go through the Frontend.
func (f *TelegramFrontend) Context() tele.Context { return f.c }

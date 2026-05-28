package bot

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"her/trace"

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

	// Trace state — lazily initialized on first TraceCallback call.
	traceOnce  sync.Once
	traceBoard *trace.Board
	traceMsg   *tele.Message
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

// --- TraceProvider implementation ---

func (f *TelegramFrontend) TraceCallback(slot string) func(text string) error {
	f.initTrace()
	return func(text string) error {
		go f.traceBoard.Set(slot, text)
		return nil
	}
}

func (f *TelegramFrontend) TraceFinalize() {
	if f.traceBoard == nil {
		return
	}
	snapshot := f.traceBoard.Snapshot()
	if snapshot == "" {
		return
	}
	f.b.lastTraceMu.Lock()
	f.b.lastTraceSnapshot = snapshot
	f.b.lastTraceMu.Unlock()

	const paginateThreshold = 3800
	if len(snapshot) > paginateThreshold && f.traceMsg != nil {
		_ = f.c.Bot().Delete(f.traceMsg)
		_ = f.b.sendPaginated(f.c, snapshot)
	}
}

func (f *TelegramFrontend) initTrace() {
	f.traceOnce.Do(f.initTraceOnce)
}

func (f *TelegramFrontend) initTraceOnce() {
	traceMsg, err := f.c.Bot().Send(f.c.Recipient(), "🧠")
	if err != nil {
		log.Warn("trace: failed to send placeholder", "err", err)
	}
	f.traceMsg = traceMsg

	edit := func(text string) {
		if text == "" {
			return
		}
		if f.traceMsg == nil {
			msg, err := f.c.Bot().Send(f.c.Recipient(), text, &tele.SendOptions{ParseMode: tele.ModeHTML})
			if err != nil {
				msg, err = f.c.Bot().Send(f.c.Recipient(), stripHTML(text))
				if err != nil {
					return
				}
			}
			f.traceMsg = msg
			return
		}
		_, err := f.c.Bot().Edit(f.traceMsg, text, &tele.SendOptions{ParseMode: tele.ModeHTML})
		if err != nil && !strings.Contains(err.Error(), "not modified") {
			_, _ = f.c.Bot().Edit(f.traceMsg, stripHTML(text))
		}
	}

	f.traceBoard = trace.NewBoard(edit)
}

// --- TTSProvider implementation ---

func (f *TelegramFrontend) SendVoice(text string) {
	f.b.sendVoiceReply(f.c, text)
}

// --- StreamProvider implementation ---

func (f *TelegramFrontend) MakeStreamCallback() (func(chunk string) error, func()) {
	var mu sync.Mutex
	var buf strings.Builder
	var lastFlushed string
	done := make(chan struct{})

	go func() {
		ticker := time.NewTicker(400 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				mu.Lock()
				current := buf.String()
				mu.Unlock()
				if current == lastFlushed || current == "" {
					continue
				}
				_, err := f.c.Bot().Edit(f.placeholder, current+"▋")
				if err != nil && !strings.Contains(err.Error(), "not modified") {
					continue
				}
				lastFlushed = current
			}
		}
	}()

	cb := func(chunk string) error {
		mu.Lock()
		buf.WriteString(chunk)
		mu.Unlock()
		return nil
	}

	stop := func() { close(done) }

	return cb, stop
}

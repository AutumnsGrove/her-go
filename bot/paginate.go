// Package bot — pagination for long Telegram messages.
//
// Telegram has a hard 4096-character limit per message. When output
// exceeds that (like /facts with 100+ facts), we split it into pages
// and attach ◀/▶ inline buttons so the user can navigate.
//
// The flow:
//   1. sendPaginated splits the text into page-sized chunks by lines
//   2. Page 1 is sent with navigation buttons (if more than one page)
//   3. The page session (all pages) is stored in Bot.pageSessions
//   4. When the user clicks ◀/▶, handlePageCallback edits the message
//      to show the requested page
//
// Pages are stored per-chat (keyed by chat ID). Only one paginated
// view is active per chat — starting a new one replaces the old one.
// This keeps things simple and avoids stale button presses on old messages.
package bot

import (
	"fmt"
	"strconv"
	"strings"

	tele "gopkg.in/telebot.v4"
)

// maxTelegramLen is Telegram's maximum message length in characters.
// We leave a small buffer for the page footer ("Page X of Y") and
// any HTML overhead from the navigation text.
const maxTelegramLen = 4000

// pageSession holds all the pages for a single paginated view.
// Stored in Bot.pageSessions, keyed by chat ID.
type pageSession struct {
	Pages []string // pre-split pages of text
}

// paginateLines splits text into pages that fit within maxLen.
// It splits on newline boundaries so we never break a line (or HTML
// tag) in the middle. If a single line exceeds maxLen on its own,
// it gets its own page (Telegram will truncate it, but that's an
// edge case — facts are capped at 200 chars).
//
// Returns a slice of page strings. If the text fits in one page,
// you get a single-element slice.
func paginateLines(text string, maxLen int) []string {
	lines := strings.Split(text, "\n")

	var pages []string
	var current strings.Builder

	for _, line := range lines {
		// Would adding this line exceed the limit?
		// +1 for the newline character we'd add.
		if current.Len() > 0 && current.Len()+len(line)+1 > maxLen {
			// Flush the current page.
			pages = append(pages, current.String())
			current.Reset()
		}
		if current.Len() > 0 {
			current.WriteString("\n")
		}
		current.WriteString(line)
	}

	// Don't forget the last page.
	if current.Len() > 0 {
		pages = append(pages, current.String())
	}

	return pages
}

// renderPage builds the final message text for a given page, including
// the "Page X of Y" footer. The footer uses HTML italic formatting.
func renderPage(pages []string, pageIdx int) string {
	if pageIdx < 0 || pageIdx >= len(pages) {
		return "Page not found."
	}

	text := pages[pageIdx]

	// Only add page indicator if there are multiple pages.
	if len(pages) > 1 {
		text += fmt.Sprintf("\n\n<i>Page %d of %d</i>", pageIdx+1, len(pages))
	}

	return text
}

// pageMarkup builds the inline keyboard for page navigation.
// Shows ◀ Prev and/or ▶ Next buttons depending on the current page.
// Returns nil if there's only one page (no buttons needed).
func pageMarkup(pageIdx, totalPages int) *tele.ReplyMarkup {
	if totalPages <= 1 {
		return nil
	}

	markup := &tele.ReplyMarkup{}
	var btns []tele.Btn

	if pageIdx > 0 {
		btns = append(btns, markup.Data(
			"◀ Prev",
			"page", // Action — routes to handlePageCallback
			strconv.Itoa(pageIdx-1), // Value — target page number
		))
	}

	if pageIdx < totalPages-1 {
		btns = append(btns, markup.Data(
			"▶ Next",
			"page",
			strconv.Itoa(pageIdx+1),
		))
	}

	markup.Inline(markup.Row(btns...))
	return markup
}

// sendPaginated splits text into pages and sends page 1 with nav
// buttons. If the text fits in a single message, it sends normally
// with no buttons. The page session is stored so handlePageCallback
// can serve subsequent pages.
//
// This is the main entry point — any handler that might produce long
// output can call this instead of c.Send().
func (b *Bot) sendPaginated(c tele.Context, text string) error {
	pages := paginateLines(text, maxTelegramLen)

	// Fast path: fits in one message, no pagination needed.
	if len(pages) == 1 {
		return c.Send(text, &tele.SendOptions{ParseMode: tele.ModeHTML})
	}

	// Store the page session for this chat.
	chatID := c.Chat().ID
	b.pageSessions.Store(chatID, &pageSession{Pages: pages})

	// Send page 1 with navigation buttons.
	rendered := renderPage(pages, 0)
	markup := pageMarkup(0, len(pages))

	opts := &tele.SendOptions{
		ParseMode:   tele.ModeHTML,
		ReplyMarkup: markup,
	}

	return c.Send(rendered, opts)
}

// handlePageCallback fires when the user clicks a ◀/▶ pagination
// button. It looks up the page session for this chat, renders the
// requested page, and edits the message in-place.
func (b *Bot) handlePageCallback(c tele.Context) error {
	chatID := c.Chat().ID

	// Look up the active page session.
	val, ok := b.pageSessions.Load(chatID)
	if !ok {
		return c.Respond(&tele.CallbackResponse{Text: "Session expired — run the command again."})
	}
	session := val.(*pageSession)

	// Parse the target page number from the callback data.
	data := strings.TrimSpace(c.Callback().Data)
	pageIdx, err := strconv.Atoi(data)
	if err != nil || pageIdx < 0 || pageIdx >= len(session.Pages) {
		return c.Respond(&tele.CallbackResponse{Text: "Invalid page."})
	}

	// Acknowledge the button press (removes the loading spinner).
	_ = c.Respond()

	// Edit the message to show the requested page with updated buttons.
	rendered := renderPage(session.Pages, pageIdx)
	markup := pageMarkup(pageIdx, len(session.Pages))

	return c.Edit(rendered, &tele.SendOptions{
		ParseMode:   tele.ModeHTML,
		ReplyMarkup: markup,
	})
}

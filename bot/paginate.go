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

	"her/tools"

	tele "gopkg.in/telebot.v4"
)

// pageFooterBuffer is the character budget reserved for the "Page X of Y"
// footer and any HTML overhead from navigation text. Subtracted from
// TelegramMaxMessageLen to get the usable content length per page.
const pageFooterBuffer = 96

// pageSession holds all the pages for a single paginated view.
// Stored in Bot.pageSessions, keyed by chat ID.
type pageSession struct {
	Pages []string // pre-split pages of text
}

// paginateWithBlocks splits text into pages, respecting block boundaries
// for place cards. Place cards (from nearby_search) start with "\n\n───\n"
// and contain multiple entries starting with "📍". This function ensures
// we never split a place card mid-entry.
//
// Returns a slice of page strings. If the text fits in one page, you get
// a single-element slice.
func paginateWithBlocks(text string, maxLen int) []string {
	// Check if this text contains place cards.
	const placeCardSeparator = "\n\n───\n"
	sepIdx := strings.Index(text, placeCardSeparator)

	if sepIdx == -1 {
		// No place cards — use line-based pagination.
		return paginateLines(text, maxLen)
	}

	// Split into chat response + place card block.
	chatResponse := text[:sepIdx]
	placeBlock := text[sepIdx:] // includes the separator

	// Split place cards on the 📍 marker. Each card is everything from
	// one 📍 to the next (or end of string). We keep the 📍 with its card.
	const cardMarker = "\n📍"
	var cards []string

	// Find the separator line first.
	separatorEnd := strings.Index(placeBlock, cardMarker)
	if separatorEnd == -1 {
		// Malformed — no cards found. Fall back to line pagination.
		return paginateLines(text, maxLen)
	}

	separator := placeBlock[:separatorEnd] // "\n\n───\n"
	remainingCards := placeBlock[separatorEnd:]

	// Split on card boundaries.
	parts := strings.Split(remainingCards, cardMarker)
	for i, part := range parts {
		if part == "" {
			continue
		}
		// Re-add the marker (except for empty first element from split).
		if i == 0 {
			cards = append(cards, part)
		} else {
			cards = append(cards, cardMarker+part)
		}
	}

	// Now build pages intelligently.
	var pages []string

	// Check if everything fits on one page.
	totalLen := len(chatResponse) + len(separator) + len(strings.Join(cards, ""))
	if totalLen <= maxLen {
		// Fast path: everything fits together.
		return []string{chatResponse + separator + strings.Join(cards, "")}
	}

	// Need to paginate. Start with chat response.
	if len(chatResponse) > maxLen {
		// Chat response itself needs multiple pages.
		chatPages := paginateLines(chatResponse, maxLen)
		pages = append(pages, chatPages...)
	} else if chatResponse != "" {
		pages = append(pages, chatResponse)
	}

	// Now add place cards. Try to pack multiple cards per page.
	var currentPage strings.Builder
	currentPage.WriteString(separator) // Start with the separator

	for _, card := range cards {
		// Would adding this card exceed the limit?
		testLen := currentPage.Len() + len(card)
		if testLen > maxLen {
			// Current page is full — flush it and start a new one.
			if currentPage.Len() > len(separator) {
				// Only flush if we have content beyond the separator.
				pages = append(pages, currentPage.String())
				currentPage.Reset()
				currentPage.WriteString(separator)
			}
			// If the card itself is too long, put it on its own page.
			if len(separator)+len(card) > maxLen {
				// This single card is too big. Add it anyway (Telegram
				// will truncate, but that's better than dropping it).
				pages = append(pages, separator+card)
				continue
			}
		}
		currentPage.WriteString(card)
	}

	// Don't forget the last page.
	if currentPage.Len() > len(separator) {
		pages = append(pages, currentPage.String())
	}

	return pages
}

// paginateLines splits text into pages that fit within maxLen.
// It splits on newline boundaries so we never break a line (or HTML
// tag) in the middle. If a single line exceeds maxLen on its own,
// it's force-split at the character boundary (edge case for unstructured text).
//
// Returns a slice of page strings. If the text fits in one page,
// you get a single-element slice.
func paginateLines(text string, maxLen int) []string {
	// Handle edge case: text with no newlines that's too long.
	if !strings.Contains(text, "\n") && len(text) > maxLen {
		// Force-split on character boundaries.
		var pages []string
		for len(text) > maxLen {
			pages = append(pages, text[:maxLen])
			text = text[maxLen:]
		}
		if len(text) > 0 {
			pages = append(pages, text)
		}
		return pages
	}

	lines := strings.Split(text, "\n")

	var pages []string
	var current strings.Builder

	for _, line := range lines {
		// If this single line exceeds maxLen, we need to split it.
		if len(line) > maxLen {
			// Flush any accumulated content first.
			if current.Len() > 0 {
				pages = append(pages, current.String())
				current.Reset()
			}

			// Force-split this long line into chunks.
			for len(line) > maxLen {
				pages = append(pages, line[:maxLen])
				line = line[maxLen:]
			}

			// Add the remainder to current (will be flushed later or at end).
			if len(line) > 0 {
				current.WriteString(line)
			}
			continue
		}

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
// output can call this instead of c.Send(). Uses block-aware splitting
// to avoid breaking place cards mid-entry.
func (b *Bot) sendPaginated(c tele.Context, text string) error {
	// Calculate usable content length: Telegram's limit minus footer buffer.
	maxContentLen := tools.TelegramMaxMessageLen - pageFooterBuffer
	pages := paginateWithBlocks(text, maxContentLen)

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

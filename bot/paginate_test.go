package bot

import (
	"strings"
	"testing"
)

// TestPaginateWithBlocks_ShortText verifies that short messages
// (under the limit) are returned as-is without pagination.
func TestPaginateWithBlocks_ShortText(t *testing.T) {
	text := "This is a short message."
	pages := paginateWithBlocks(text, 4000)

	if len(pages) != 1 {
		t.Errorf("expected 1 page for short text, got %d", len(pages))
	}
	if pages[0] != text {
		t.Errorf("page content doesn't match input")
	}
}

// TestPaginateWithBlocks_EmptyString verifies empty input
// doesn't cause a panic or return unexpected results.
func TestPaginateWithBlocks_EmptyString(t *testing.T) {
	pages := paginateWithBlocks("", 4000)

	// Empty string should return either 0 pages or 1 empty page.
	// Either is acceptable as long as it doesn't panic.
	if len(pages) > 1 {
		t.Errorf("empty string returned %d pages, expected 0 or 1", len(pages))
	}
}

// TestPaginateWithBlocks_NoPlaceCards verifies that text without
// place cards falls back to line-based pagination correctly.
func TestPaginateWithBlocks_NoPlaceCards(t *testing.T) {
	// Build a long message with no place card separator.
	var lines []string
	for i := 0; i < 200; i++ {
		lines = append(lines, "This is line number "+string(rune(i))+" with some content.")
	}
	text := strings.Join(lines, "\n")

	pages := paginateWithBlocks(text, 1000)

	// Should split into multiple pages.
	if len(pages) < 2 {
		t.Errorf("long text without place cards should paginate, got %d pages", len(pages))
	}

	// Each page should be under the limit.
	for i, page := range pages {
		if len(page) > 1000 {
			t.Errorf("page %d exceeds limit: %d chars", i, len(page))
		}
	}

	// Concatenating all pages should reconstruct the original text.
	reconstructed := strings.Join(pages, "\n")
	if reconstructed != text {
		t.Error("concatenated pages don't match original text")
	}
}

// TestPaginateWithBlocks_SinglePlaceCard verifies a single place
// card is kept intact, even if it exceeds the page limit.
func TestPaginateWithBlocks_SinglePlaceCard(t *testing.T) {
	chatResponse := "Here's a place near you:"
	placeCard := "\n\n───\n📍 Blue Bottle Coffee (Coffee Shop) — 350m away\n   123 Main St, Portland, OR 97214\n   → https://maps.google.com/?q=45.523,-122.676"

	text := chatResponse + placeCard

	pages := paginateWithBlocks(text, 4000)

	if len(pages) != 1 {
		t.Errorf("single short place card should fit on 1 page, got %d", len(pages))
	}

	if pages[0] != text {
		t.Error("page content doesn't match original")
	}
}

// TestPaginateWithBlocks_SingleVeryLongPlaceCard verifies that
// a place card that exceeds the page limit gets its own page
// (and Telegram will truncate it, but we don't drop it).
func TestPaginateWithBlocks_SingleVeryLongPlaceCard(t *testing.T) {
	chatResponse := "Here's a place:"
	// Create a card with a very long address that exceeds the limit.
	veryLongAddress := strings.Repeat("Very Long Street Name ", 200)
	placeCard := "\n\n───\n📍 Place Name\n   " + veryLongAddress + "\n   → https://maps.google.com/"

	text := chatResponse + placeCard

	pages := paginateWithBlocks(text, 500)

	// Should split: chat response on page 1, giant card on page 2.
	if len(pages) < 2 {
		t.Errorf("very long place card should be split, got %d pages", len(pages))
	}

	// First page should be the chat response.
	if !strings.Contains(pages[0], chatResponse) {
		t.Error("first page should contain chat response")
	}

	// Second page should contain the card.
	if !strings.Contains(pages[1], "Place Name") {
		t.Error("second page should contain the place card")
	}
}

// TestPaginateWithBlocks_MultiplePlaceCards verifies that multiple
// place cards are correctly split across pages without breaking
// individual cards.
func TestPaginateWithBlocks_MultiplePlaceCards(t *testing.T) {
	chatResponse := "Here are some places nearby:"

	// Build 10 place cards, each ~150 chars.
	var cards []string
	for i := 0; i < 10; i++ {
		card := "\n📍 Coffee Shop " + string(rune('A'+i)) + " (Cafe) — 500m away\n   " +
			string(rune('0'+i)) + "23 Main St, Portland, OR\n   → https://maps.google.com/?q=45.5,122.6"
		cards = append(cards, card)
	}

	placeBlock := "\n\n───\n" + strings.Join(cards, "")
	text := chatResponse + placeBlock

	pages := paginateWithBlocks(text, 600)

	// Should split into multiple pages.
	if len(pages) < 2 {
		t.Errorf("multiple place cards should paginate, got %d pages", len(pages))
	}

	// Every page with cards should start with the separator.
	for i, page := range pages {
		if i == 0 {
			// First page is the chat response.
			continue
		}
		if !strings.HasPrefix(page, "\n\n───\n") {
			t.Errorf("page %d with cards should start with separator", i)
		}
	}

	// No card should be split. Check that every "📍" has its complete content.
	for i, page := range pages {
		cardStarts := strings.Count(page, "\n📍")
		if cardStarts > 0 {
			// This page has cards. Count the number of map links — should match.
			mapLinks := strings.Count(page, "→ https://")
			if cardStarts != mapLinks {
				t.Errorf("page %d: %d cards but %d map links — card was split", i, cardStarts, mapLinks)
			}
		}
	}
}

// TestPaginateWithBlocks_LongChatAndPlaceCards verifies that
// both the chat response and place cards paginate correctly when
// both are too long.
func TestPaginateWithBlocks_LongChatAndPlaceCards(t *testing.T) {
	// Long chat response that needs multiple pages.
	var chatLines []string
	for i := 0; i < 50; i++ {
		chatLines = append(chatLines, "This is a very detailed explanation about the search results. "+strings.Repeat("More text ", 10))
	}
	chatResponse := strings.Join(chatLines, "\n")

	// Multiple place cards.
	var cards []string
	for i := 0; i < 8; i++ {
		card := "\n📍 Location " + string(rune('A'+i)) + "\n   Address line\n   → https://maps.google.com/"
		cards = append(cards, card)
	}
	placeBlock := "\n\n───\n" + strings.Join(cards, "")

	text := chatResponse + placeBlock

	pages := paginateWithBlocks(text, 800)

	// Should have multiple pages.
	if len(pages) < 3 {
		t.Errorf("long chat + cards should create many pages, got %d", len(pages))
	}

	// First pages should be chat content (no separator).
	firstPageHasSeparator := strings.Contains(pages[0], "\n\n───\n")
	if firstPageHasSeparator {
		t.Error("first page should be chat content, not place cards")
	}

	// Later pages should have cards with separator.
	lastPageHasSeparator := strings.Contains(pages[len(pages)-1], "\n\n───\n")
	if !lastPageHasSeparator {
		t.Error("last page should contain place cards with separator")
	}
}

// TestPaginateWithBlocks_MalformedPlaceCards verifies that
// malformed place card structures fall back to line pagination
// gracefully without panicking.
func TestPaginateWithBlocks_MalformedPlaceCards(t *testing.T) {
	// Separator but no actual cards.
	text1 := "Chat response\n\n───\n"
	pages1 := paginateWithBlocks(text1, 4000)
	if len(pages1) == 0 {
		t.Error("malformed place cards (no cards) should still return pages")
	}

	// Separator but missing card marker.
	text2 := "Chat response\n\n───\nSome text but no card marker"
	pages2 := paginateWithBlocks(text2, 4000)
	if len(pages2) == 0 {
		t.Error("malformed place cards (missing marker) should still return pages")
	}
}

// TestPaginateWithBlocks_ExactBoundary verifies behavior when
// text is exactly at the page limit.
func TestPaginateWithBlocks_ExactBoundary(t *testing.T) {
	limit := 500
	text := strings.Repeat("x", limit)

	pages := paginateWithBlocks(text, limit)

	if len(pages) != 1 {
		t.Errorf("text exactly at limit should be 1 page, got %d", len(pages))
	}
}

// TestPaginateWithBlocks_OneCharOverBoundary verifies behavior
// when text is just 1 char over the limit.
func TestPaginateWithBlocks_OneCharOverBoundary(t *testing.T) {
	limit := 500
	text := strings.Repeat("x", limit+1)

	pages := paginateWithBlocks(text, limit)

	if len(pages) < 2 {
		t.Errorf("text 1 char over limit should split into 2 pages, got %d", len(pages))
	}
}

// TestPaginateWithBlocks_PlaceCardPackingEfficiency verifies that
// multiple small place cards are packed efficiently onto pages
// rather than wasting space.
func TestPaginateWithBlocks_PlaceCardPackingEfficiency(t *testing.T) {
	chatResponse := "Results:"

	// Create 5 small cards that should all fit on one page together.
	var cards []string
	for i := 0; i < 5; i++ {
		card := "\n📍 Cafe " + string(rune('A'+i)) + "\n   123 St\n   → link"
		cards = append(cards, card)
	}
	placeBlock := "\n\n───\n" + strings.Join(cards, "")

	text := chatResponse + placeBlock

	// Use a limit that should fit all 5 cards + separator.
	totalLen := len(text)
	pages := paginateWithBlocks(text, totalLen+100)

	// Should not over-paginate — all cards should fit together.
	if len(pages) > 2 {
		t.Errorf("small cards should pack efficiently, got %d pages for %d chars (limit %d)",
			len(pages), totalLen, totalLen+100)
	}
}

// TestPaginateWithBlocks_PreserveCardOrder verifies that place
// cards appear in the same order across pages as in the input.
func TestPaginateWithBlocks_PreserveCardOrder(t *testing.T) {
	chatResponse := "Ordered results:"

	var cards []string
	for i := 0; i < 10; i++ {
		card := "\n📍 Place " + string(rune('0'+i)) + "\n   Address\n   → link"
		cards = append(cards, card)
	}
	placeBlock := "\n\n───\n" + strings.Join(cards, "")

	text := chatResponse + placeBlock

	pages := paginateWithBlocks(text, 300)

	// Extract all card names from pages in order.
	var foundOrder []string
	for _, page := range pages {
		for i := 0; i < 10; i++ {
			needle := "📍 Place " + string(rune('0'+i))
			if strings.Contains(page, needle) {
				foundOrder = append(foundOrder, needle)
			}
		}
	}

	// Should find all 10 cards in order.
	if len(foundOrder) != 10 {
		t.Errorf("expected to find 10 cards, found %d", len(foundOrder))
	}

	// Verify order is preserved.
	for i := 0; i < 10; i++ {
		expected := "📍 Place " + string(rune('0'+i))
		if foundOrder[i] != expected {
			t.Errorf("card order violated at position %d: got %q, expected %q",
				i, foundOrder[i], expected)
		}
	}
}

// TestPaginateWithBlocks_HTMLSpecialChars verifies that HTML
// entities and special characters in place cards don't break
// pagination logic.
func TestPaginateWithBlocks_HTMLSpecialChars(t *testing.T) {
	chatResponse := "Here's a place with <special> & \"chars\":"
	placeCard := "\n\n───\n📍 Café & Bistró <Premium> — 1km\n   Street & Avenue\n   → https://link?foo=bar&baz=qux"

	text := chatResponse + placeCard

	pages := paginateWithBlocks(text, 4000)

	if len(pages) == 0 {
		t.Error("HTML special chars should not break pagination")
	}

	// Verify the special chars are preserved.
	reconstructed := strings.Join(pages, "")
	if !strings.Contains(reconstructed, "<special>") || !strings.Contains(reconstructed, "Café") {
		t.Error("special characters were lost during pagination")
	}
}

// TestPaginateWithBlocks_PageSizeLimits verifies that no page
// exceeds the specified limit (except when a single atomic unit
// like a card is too big).
func TestPaginateWithBlocks_PageSizeLimits(t *testing.T) {
	// Build a large mixed message.
	var chatLines []string
	for i := 0; i < 100; i++ {
		chatLines = append(chatLines, "Line "+strings.Repeat("content ", 20))
	}
	chatResponse := strings.Join(chatLines, "\n")

	var cards []string
	for i := 0; i < 15; i++ {
		card := "\n📍 Place " + string(rune('A'+i)) + "\n   Address line\n   → link"
		cards = append(cards, card)
	}
	placeBlock := "\n\n───\n" + strings.Join(cards, "")

	text := chatResponse + placeBlock
	limit := 1000

	pages := paginateWithBlocks(text, limit)

	for i, page := range pages {
		// Pages should generally be under the limit. Allow some tolerance
		// for atomic units (cards) that can't be split.
		if len(page) > limit*2 {
			t.Errorf("page %d is unreasonably large: %d chars (limit %d)", i, len(page), limit)
		}
	}
}

// TestPaginateLines_BasicSplit verifies the line-based pagination
// fallback works correctly.
func TestPaginateLines_BasicSplit(t *testing.T) {
	var lines []string
	for i := 0; i < 50; i++ {
		lines = append(lines, "This is line number "+strings.Repeat("x", 50))
	}
	text := strings.Join(lines, "\n")

	pages := paginateLines(text, 500)

	if len(pages) < 2 {
		t.Errorf("long text should split into multiple pages, got %d", len(pages))
	}

	// Verify pages don't exceed limit.
	for i, page := range pages {
		if len(page) > 500 {
			t.Errorf("page %d exceeds limit: %d chars", i, len(page))
		}
	}

	// Verify reconstruction.
	reconstructed := strings.Join(pages, "\n")
	if reconstructed != text {
		t.Error("reconstructed text doesn't match original")
	}
}

// TestPaginateLines_SingleLineTooLong verifies that a single line
// that exceeds the limit gets its own page (edge case handling).
func TestPaginateLines_SingleLineTooLong(t *testing.T) {
	hugeLine := strings.Repeat("x", 1000)
	text := "Short line\n" + hugeLine + "\nAnother short line"

	pages := paginateLines(text, 200)

	if len(pages) < 3 {
		t.Errorf("huge line should force multiple pages, got %d", len(pages))
	}

	// The huge line should be on its own page.
	foundHugeLine := false
	for _, page := range pages {
		if strings.Contains(page, hugeLine) {
			foundHugeLine = true
			// This page should ONLY contain the huge line (no other content).
			if strings.Contains(page, "Short line") || strings.Contains(page, "Another short") {
				t.Error("huge line should be isolated on its own page")
			}
		}
	}

	if !foundHugeLine {
		t.Error("huge line not found in any page")
	}
}

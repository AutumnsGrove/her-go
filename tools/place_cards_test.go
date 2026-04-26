// Package tools — tests for FormatPlaceCards.
//
// FormatPlaceCards renders the structured block that gets appended after
// the chat model's reply. The format must be stable — the user sees this
// directly in Telegram, so changes here break the visual contract.
package tools

import (
	"strings"
	"testing"
)

func TestFormatPlaceCards_Empty(t *testing.T) {
	got := FormatPlaceCards(nil)
	if got != "" {
		t.Errorf("FormatPlaceCards(nil) = %q, want empty string", got)
	}

	got = FormatPlaceCards([]PlaceCard{})
	if got != "" {
		t.Errorf("FormatPlaceCards([]) = %q, want empty string", got)
	}
}

func TestFormatPlaceCards_SingleCard(t *testing.T) {
	cards := []PlaceCard{
		{
			Name:         "Blue Bottle Coffee",
			Category:     "Coffee Shop",
			DistanceText: "350m away",
			Address:      "123 Main St, Portland, OR",
			MapsURL:      "https://maps.google.com/?q=45.523,-122.676",
		},
	}

	got := FormatPlaceCards(cards)

	checks := []struct {
		label  string
		substr string
	}{
		{"separator", "───"},
		{"emoji", "📍"},
		{"name", "Blue Bottle Coffee"},
		{"category", "(Coffee Shop)"},
		{"distance", "350m away"},
		{"address", "123 Main St, Portland, OR"},
		{"maps_link", "https://maps.google.com/?q=45.523,-122.676"},
		{"arrow", "→"},
	}

	for _, c := range checks {
		if !strings.Contains(got, c.substr) {
			t.Errorf("FormatPlaceCards missing %s: want %q in:\n%s", c.label, c.substr, got)
		}
	}
}

func TestFormatPlaceCards_MultipleCards(t *testing.T) {
	cards := []PlaceCard{
		{Name: "First", DistanceText: "100m away"},
		{Name: "Second", DistanceText: "200m away"},
		{Name: "Third", DistanceText: "300m away"},
	}

	got := FormatPlaceCards(cards)

	// All three names should appear.
	for _, name := range []string{"First", "Second", "Third"} {
		if !strings.Contains(got, name) {
			t.Errorf("FormatPlaceCards missing place %q in:\n%s", name, got)
		}
	}

	// Should only have ONE separator line (at the top, not between cards).
	if strings.Count(got, "───") != 1 {
		t.Errorf("FormatPlaceCards should have exactly 1 separator, got %d in:\n%s",
			strings.Count(got, "───"), got)
	}
}

func TestFormatPlaceCards_NoCategory(t *testing.T) {
	cards := []PlaceCard{
		{Name: "Mystery Place", DistanceText: "nearby"},
	}

	got := FormatPlaceCards(cards)

	// Should NOT have empty parens "()" when category is empty.
	if strings.Contains(got, "()") {
		t.Errorf("FormatPlaceCards should not show empty parens:\n%s", got)
	}
}

func TestFormatPlaceCards_NoAddress(t *testing.T) {
	cards := []PlaceCard{
		{Name: "Secret Spot", DistanceText: "nearby"},
	}

	got := FormatPlaceCards(cards)

	// Should not have an indented address line.
	lines := strings.Split(got, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || trimmed == "───" {
			continue
		}
		// Only the name line and possibly the arrow line should exist.
		if strings.HasPrefix(line, "   ") && !strings.HasPrefix(trimmed, "→") {
			t.Errorf("FormatPlaceCards should not have address line when address is empty:\n%s", got)
		}
	}
}

func TestFormatPlaceCards_NoMapsURL(t *testing.T) {
	cards := []PlaceCard{
		{Name: "No Coords", Address: "123 Main St"},
	}

	got := FormatPlaceCards(cards)

	// Should not have an arrow/link line.
	if strings.Contains(got, "→") {
		t.Errorf("FormatPlaceCards should not show arrow when MapsURL is empty:\n%s", got)
	}
}

func TestFormatPlaceCards_StartsWithNewlines(t *testing.T) {
	cards := []PlaceCard{
		{Name: "Test"},
	}

	got := FormatPlaceCards(cards)

	// Should start with newlines to separate from the chat model's prose.
	if !strings.HasPrefix(got, "\n\n") {
		t.Errorf("FormatPlaceCards should start with \\n\\n for separation, got %q", got[:10])
	}
}

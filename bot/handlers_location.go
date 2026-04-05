// handlers_location.go handles Telegram location and venue shares.
//
// When the user drops a pin or shares a venue, we:
//  1. Save it to location_history (for future analysis + nearby_search fallback)
//  2. Run the agent pipeline with a synthetic message so Mira can respond
//     naturally ("nice, you're at the library — want me to find a coffee
//     shop nearby?")
package bot

import (
	"fmt"

	tele "gopkg.in/telebot.v4"
)

// handleLocation processes a Telegram location share (pin drop).
// The message has lat/lon but no text — we build a synthetic prompt
// and run it through the full agent pipeline.
func (b *Bot) handleLocation(c tele.Context) error {
	msg := c.Message()
	if msg.Location == nil {
		return nil
	}

	lat := msg.Location.Lat
	lon := msg.Location.Lng

	conversationID := b.getConversationID(msg.Chat.ID)
	label := fmt.Sprintf("%.4f, %.4f", lat, lon)

	log.Infof("─── incoming location ───")
	log.Infof("  pin: %s", label)

	// Save to location_history.
	if err := b.store.InsertLocation(float64(lat), float64(lon), "", "pin", conversationID); err != nil {
		log.Error("saving location to history", "err", err)
	}

	// Build a synthetic message for the agent.
	syntheticText := fmt.Sprintf("[User shared their location: %.6f, %.6f]", lat, lon)

	return b.runLocationAgent(c, syntheticText, conversationID)
}

// handleVenue processes a Telegram venue share (a named place with address).
// Venues have richer data than raw pins — name, address, and coordinates.
func (b *Bot) handleVenue(c tele.Context) error {
	msg := c.Message()
	if msg.Venue == nil {
		return nil
	}

	lat := msg.Venue.Location.Lat
	lon := msg.Venue.Location.Lng
	name := msg.Venue.Title
	address := msg.Venue.Address

	conversationID := b.getConversationID(msg.Chat.ID)

	log.Infof("─── incoming venue ───")
	log.Infof("  venue: %s (%s)", name, address)

	// Build a descriptive label for location_history.
	label := name
	if address != "" {
		label += ", " + address
	}

	// Save to location_history.
	if err := b.store.InsertLocation(float64(lat), float64(lon), label, "venue", conversationID); err != nil {
		log.Error("saving venue to history", "err", err)
	}

	// Build a synthetic message with the venue details.
	syntheticText := fmt.Sprintf("[User shared their location: %s, %s (%.6f, %.6f)]", name, address, lat, lon)

	return b.runLocationAgent(c, syntheticText, conversationID)
}

// runLocationAgent runs the agent pipeline with a synthetic location message.
// No PII scrubbing needed — coordinates and venue names aren't sensitive
// data in our tiered model. The shared runAgent handles all the UI boilerplate.
func (b *Bot) runLocationAgent(c tele.Context, syntheticText, conversationID string) error {
	// Log the synthetic message to the DB.
	msgID, err := b.store.SaveMessage("user", syntheticText, "", conversationID)
	if err != nil {
		log.Error("saving location message", "err", err)
	}

	return b.runAgent(c, AgentInput{
		UserMessage:    syntheticText,
		ConversationID: conversationID,
		TriggerMsgID:   msgID,
	})
}

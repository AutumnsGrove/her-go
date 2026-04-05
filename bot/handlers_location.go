// handlers_location.go handles Telegram location and venue shares.
//
// When the user drops a pin or shares a venue, we:
//  1. Save it to location_history (for future analysis + nearby_search fallback)
//  2. Run the agent pipeline with a synthetic message so Mira can respond
//     naturally ("nice, you're at the library — want me to find a coffee
//     shop nearby?")
//
// This duplicates the agent pipeline boilerplate from handleMessage.
// TODO: extract a shared runAgent helper (see refactor discussion).
package bot

import (
	"fmt"
	"time"

	"her/agent"
	"her/scrub"
	"her/tools"
	"her/tui"

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
// This duplicates the boilerplate from handleMessage — placeholder creation,
// callback wiring, agent.Run, cleanup. Will be eliminated when we extract
// a shared runAgent helper.
func (b *Bot) runLocationAgent(c tele.Context, syntheticText, conversationID string) error {
	// Log the synthetic message to the DB.
	msgID, err := b.store.SaveMessage("user", syntheticText, "", conversationID)
	if err != nil {
		log.Error("saving location message", "err", err)
	}

	// No PII scrubbing needed — coordinates and venue names aren't
	// sensitive data in our tiered model. Create a passthrough vault.
	scrubResult := &scrub.ScrubResult{
		Text:  syntheticText,
		Vault: scrub.NewVault(),
	}

	// Typing indicator.
	stopTyping := make(chan struct{})
	go func() {
		_ = c.Notify(tele.Typing)
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopTyping:
				return
			case <-ticker.C:
				_ = c.Notify(tele.Typing)
			}
		}
	}()

	// Trace callback (if enabled).
	var traceCallback tools.TraceCallback
	if b.cfg.Agent.Trace {
		traceCallback = b.makeTraceCallback(c)
	}

	// Placeholder message.
	placeholder, sendErr := c.Bot().Send(c.Recipient(), "\U0001F4AD")
	if sendErr != nil {
		close(stopTyping)
		log.Error("sending placeholder", "err", sendErr)
		return c.Send("Sorry, I'm having trouble right now. Try again in a moment?")
	}

	statusCallback := func(status string) error {
		_, err := c.Bot().Edit(placeholder, status)
		return err
	}
	sendCallback := func(text string) error {
		_, err := c.Bot().Send(c.Recipient(), text, &tele.SendOptions{ParseMode: tele.ModeHTML})
		return err
	}
	sendConfirmCallback := func(text string) (int64, error) {
		markup := &tele.ReplyMarkup{}
		btnYes := markup.Data("Yes", "confirm", "yes")
		btnNo := markup.Data("No", "confirm", "no")
		markup.Inline(markup.Row(btnYes, btnNo))
		sent, err := c.Bot().Send(c.Recipient(), text, markup)
		if err != nil {
			return 0, err
		}
		return int64(sent.ID), nil
	}
	stageResetCallback := func() error {
		newPlaceholder, err := c.Bot().Send(c.Recipient(), "\U0001F4AD")
		if err != nil {
			return err
		}
		placeholder = newPlaceholder
		return nil
	}
	deletePlaceholderCallback := func() error {
		return c.Bot().Delete(placeholder)
	}

	var ttsCallback tools.TTSCallback
	if b.ttsClient != nil && b.ttsClient.ReplyMode() == "voice" {
		ttsCallback = func(text string) {
			b.sendVoiceReply(c, text)
		}
	}

	turnStart := time.Now()
	if b.eventBus != nil {
		b.eventBus.Emit(tui.TurnStartEvent{
			Time:           turnStart,
			TurnID:         msgID,
			UserMessage:    truncate(syntheticText, 100),
			ConversationID: conversationID,
		})
	}

	b.agentBusy.Store(true)
	result, err := agent.Run(agent.RunParams{
		AgentLLM:                  b.agentLLM,
		ChatLLM:                   b.llm,
		VisionLLM:                 b.visionLLM,
		ClassifierLLM:             b.classifierLLM,
		Store:                     b.store,
		EmbedClient:               b.embedClient,
		SimilarityThreshold:       b.cfg.Embed.SimilarityThreshold,
		TavilyClient:              b.tavilyClient,
		WeatherClient:             b.weatherClient,
		Cfg:                       b.cfg,
		ScrubbedUserMessage:       scrubResult.Text,
		ScrubVault:                scrubResult.Vault,
		ConversationID:            conversationID,
		TriggerMsgID:              msgID,
		StatusCallback:            statusCallback,
		SendCallback:              sendCallback,
		StageResetCallback:        stageResetCallback,
		DeletePlaceholderCallback: deletePlaceholderCallback,
		SendConfirmCallback:       sendConfirmCallback,
		TTSCallback:               ttsCallback,
		TraceCallback:             traceCallback,
		ReflectionThreshold:       b.cfg.Persona.ReflectionMemoryThreshold,
		RewriteEveryN:             b.cfg.Persona.RewriteEveryNReflections,
		EventBus:                  b.eventBus,
		ConfigPath:                b.configPath,
		SkillRegistry:             b.skillRegistry,
	})
	b.agentBusy.Store(false)

	close(stopTyping)

	if err != nil {
		log.Error("agent error (location)", "err", err)
		_, _ = c.Bot().Edit(placeholder, "Sorry, I'm having trouble thinking right now. Try again in a moment?")
		return nil
	}

	log.Infof("  %s: %s", b.cfg.Identity.Her, truncate(result.ReplyText, 100))
	log.Info("─── reply sent ───")

	if b.eventBus != nil {
		b.eventBus.Emit(tui.TurnEndEvent{
			Time:       time.Now(),
			TurnID:     msgID,
			ElapsedMs:  time.Since(turnStart).Milliseconds(),
			TotalCost:  result.TotalCost,
			ToolCalls:  result.ToolCalls,
			FactsSaved: result.FactsSaved,
		})
	}

	return nil
}

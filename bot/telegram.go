// Package bot handles the Telegram interface — receiving messages,
// running them through the pipeline (log → scrub → LLM → reply),
// and managing the typing indicator.
package bot

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"her-go/config"
	"her-go/llm"
	"her-go/memory"
	"her-go/scrub"

	tele "gopkg.in/telebot.v4"
)

// Bot wraps the Telegram bot and all its dependencies.
// This is a common Go pattern: a "god struct" that holds references
// to all the services a component needs. Similar to dependency injection
// in Python/Java, but done manually (Go favors explicitness over magic).
type Bot struct {
	tb           *tele.Bot
	llm          *llm.Client
	store        *memory.Store
	cfg          *config.Config
	systemPrompt string

	// conversationIDs tracks the active conversation ID per chat.
	// When /clear is called, we rotate to a new ID so the history
	// window starts fresh. sync.Map is Go's concurrent-safe map —
	// like a regular dict but safe to read/write from multiple
	// goroutines without explicit locking.
	conversationIDs sync.Map
}

// New creates and configures a new Telegram bot.
func New(cfg *config.Config, llmClient *llm.Client, store *memory.Store) (*Bot, error) {
	// tele.Settings configures the bot's behavior.
	// Poller controls how the bot receives updates from Telegram.
	settings := tele.Settings{
		Token:  cfg.Telegram.Token,
		Poller: &tele.LongPoller{Timeout: 10 * time.Second},
	}

	tb, err := tele.NewBot(settings)
	if err != nil {
		return nil, fmt.Errorf("creating telegram bot: %w", err)
	}

	// Load the base system prompt from prompt.md.
	promptBytes, err := os.ReadFile(cfg.Persona.PromptFile)
	if err != nil {
		return nil, fmt.Errorf("reading system prompt from %s: %w", cfg.Persona.PromptFile, err)
	}

	bot := &Bot{
		tb:           tb,
		llm:          llmClient,
		store:        store,
		cfg:          cfg,
		systemPrompt: string(promptBytes),
	}

	// Register command handlers. In telebot, commands like "/clear" are
	// registered separately from regular text messages. The framework
	// strips the leading "/" for you.
	tb.Handle("/clear", bot.handleClear)

	// Register message handlers. In telebot, you register handlers for
	// different event types. tele.OnText fires for any text message.
	// This is like a route decorator in Flask: @app.route("/")
	tb.Handle(tele.OnText, bot.handleMessage)

	return bot, nil
}

// Start begins polling Telegram for messages. This blocks forever
// (or until the bot is stopped), so it's typically the last thing
// called in main.go.
func (b *Bot) Start() {
	log.Println("Bot is running. Listening for messages...")
	b.tb.Start()
}

// Stop gracefully shuts down the bot.
func (b *Bot) Stop() {
	b.tb.Stop()
}

// handleMessage is the core pipeline — this is where every text message
// flows through. The spec's message flow (steps 1-11) happens here.
func (b *Bot) handleMessage(c tele.Context) error {
	msg := c.Message()
	userText := msg.Text

	// Get the active conversation ID for this chat.
	conversationID := b.getConversationID(msg.Chat.ID)

	// Step 3: Log the raw message to SQLite.
	msgID, err := b.store.SaveMessage("user", userText, "", conversationID)
	if err != nil {
		log.Printf("Error saving user message: %v", err)
		// Continue anyway — don't fail the whole pipeline because logging broke.
	}

	// Step 4: PII scrub the message.
	var scrubResult *scrub.ScrubResult
	if b.cfg.Scrub.Enabled {
		scrubResult = scrub.Scrub(userText)
	} else {
		scrubResult = &scrub.ScrubResult{
			Text:  userText,
			Vault: scrub.NewVault(),
		}
	}

	// Update the saved message with the scrubbed version.
	// We saved the raw version first (step 3), now we add the scrubbed copy.
	if msgID > 0 {
		b.store.UpdateMessageScrubbed(msgID, scrubResult.Text)

		// Persist vault entries to SQLite for audit trail.
		for _, entry := range scrubResult.Vault.Entries() {
			if err := b.store.SavePIIVaultEntry(msgID, entry.Token, entry.Original, entry.EntityType); err != nil {
				log.Printf("Error saving PII vault entry: %v", err)
			}
		}
	}

	// Step 5: Retrieve recent conversation history for context.
	recentMsgs, err := b.store.RecentMessages(conversationID, b.cfg.Memory.RecentMessages)
	if err != nil {
		log.Printf("Error retrieving recent messages: %v", err)
		recentMsgs = nil // continue without history
	}

	// Step 6: Assemble the full prompt.
	llmMessages := b.buildPrompt(scrubResult.Text, recentMsgs)

	// Step 7: Send typing indicator.
	// We start a goroutine that re-sends the typing action every 4 seconds
	// to keep the indicator alive while we wait for the LLM response.
	// Goroutines are like asyncio.create_task() but backed by real
	// lightweight threads managed by the Go runtime. They're incredibly
	// cheap — you can spawn thousands of them.
	stopTyping := make(chan struct{})
	go func() {
		// Send typing indicator immediately.
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

	// Step 8: Call the LLM.
	start := time.Now()
	resp, err := b.llm.ChatCompletion(llmMessages)
	close(stopTyping) // stop the typing indicator goroutine
	latencyMs := int(time.Since(start).Milliseconds())

	if err != nil {
		log.Printf("Error calling LLM: %v", err)
		return c.Send("Sorry, I'm having trouble thinking right now. Try again in a moment?")
	}

	// Step 9: Log the response to SQLite.
	respID, err := b.store.SaveMessage("assistant", resp.Content, resp.Content, conversationID)
	if err != nil {
		log.Printf("Error saving assistant message: %v", err)
	}

	// Update token counts on both messages now that we have usage data.
	if msgID > 0 {
		if err := b.store.UpdateMessageTokenCount(msgID, resp.PromptTokens); err != nil {
			log.Printf("Error updating user message token count: %v", err)
		}
	}
	if respID > 0 {
		if err := b.store.UpdateMessageTokenCount(respID, resp.CompletionTokens); err != nil {
			log.Printf("Error updating assistant message token count: %v", err)
		}
	}

	// Log metrics — cost comes directly from OpenRouter's response.
	if respID > 0 {
		if err := b.store.SaveMetric(resp.Model, resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens, resp.CostUSD, latencyMs, respID); err != nil {
			log.Printf("Error saving metrics: %v", err)
		}
	}

	// Step 10: Deanonymize the response (replace [PHONE_1] etc. with originals)
	// and send it back to the user.
	replyText := scrub.Deanonymize(resp.Content, scrubResult.Vault)

	return c.Send(replyText)
}

// buildPrompt assembles the layered prompt from system prompt + history + current message.
// For v0.1, we use: prompt.md + recent messages + current message.
// v0.2 will add persona.md + reflections + facts.
func (b *Bot) buildPrompt(currentMessage string, history []memory.Message) []llm.ChatMessage {
	messages := []llm.ChatMessage{
		{Role: "system", Content: b.buildSystemPrompt()},
	}

	// Add conversation history. We use the scrubbed versions so the LLM
	// never sees raw PII from past messages either.
	for _, msg := range history {
		content := msg.ContentScrubbed
		if content == "" {
			content = msg.ContentRaw // fallback if scrubbing wasn't enabled
		}
		messages = append(messages, llm.ChatMessage{
			Role:    msg.Role,
			Content: content,
		})
	}

	// Add the current (scrubbed) message.
	messages = append(messages, llm.ChatMessage{
		Role:    "user",
		Content: currentMessage,
	})

	return messages
}

// getConversationID returns the active conversation ID for a chat.
// If no conversation has been started (or after a /clear), it creates
// a new one with a timestamp suffix.
func (b *Bot) getConversationID(chatID int64) string {
	key := fmt.Sprintf("%d", chatID)

	// Load existing ID, or create a new one if none exists.
	// sync.Map.LoadOrStore is atomic — if two goroutines race here,
	// only one value gets stored. Same idea as Python's
	// dict.setdefault() but thread-safe.
	val, _ := b.conversationIDs.LoadOrStore(key, fmt.Sprintf("tg_%d_%d", chatID, time.Now().Unix()))
	return val.(string) // type assertion: sync.Map stores interface{}, we know it's a string
}

// handleClear resets the conversation context. Old messages stay in the
// DB but won't be included in future prompts since the conversation ID changes.
func (b *Bot) handleClear(c tele.Context) error {
	chatID := c.Message().Chat.ID
	key := fmt.Sprintf("%d", chatID)

	// Store a new conversation ID with a fresh timestamp.
	newID := fmt.Sprintf("tg_%d_%d", chatID, time.Now().Unix())
	b.conversationIDs.Store(key, newID)

	log.Printf("Conversation cleared for chat %d, new ID: %s", chatID, newID)
	return c.Send("Context cleared. Fresh start!")
}

// buildSystemPrompt assembles the system prompt by reading prompt.md
// fresh from disk each time. This makes it hot-reloadable — you can
// edit the prompt while the bot is running and changes take effect
// on the next message, no restart needed.
func (b *Bot) buildSystemPrompt() string {
	var parts []string

	// Read prompt.md fresh from disk each call (hot-reload).
	// Fall back to the version loaded at startup if the read fails.
	if promptBytes, err := os.ReadFile(b.cfg.Persona.PromptFile); err == nil {
		parts = append(parts, string(promptBytes))
	} else {
		parts = append(parts, b.systemPrompt)
	}

	// Load persona.md if it exists (optional for v0.1).
	if personaBytes, err := os.ReadFile(b.cfg.Persona.PersonaFile); err == nil {
		parts = append(parts, string(personaBytes))
	}

	return strings.Join(parts, "\n\n---\n\n")
}

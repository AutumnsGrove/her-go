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

	"her-go/agent"
	"her-go/config"
	"her-go/embed"
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
	agentLLM     *llm.Client    // background tool-calling brain
	embedClient  *embed.Client  // local embedding model for similarity
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
func New(cfg *config.Config, llmClient *llm.Client, agentLLM *llm.Client, embedClient *embed.Client, store *memory.Store) (*Bot, error) {
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
		agentLLM:     agentLLM,
		embedClient:  embedClient,
		store:        store,
		cfg:          cfg,
		systemPrompt: string(promptBytes),
	}

	// Register command handlers. In telebot, commands like "/clear" are
	// registered separately from regular text messages. The framework
	// strips the leading "/" for you.
	tb.Handle("/clear", bot.handleClear)
	tb.Handle("/stats", bot.handleStats)
	tb.Handle("/forget", bot.handleForget)
	tb.Handle("/facts", bot.handleFacts)

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

// truncate shortens a string for log output, adding "..." if it was cut.
func truncate(s string, maxLen int) string {
	s = strings.ReplaceAll(s, "\n", " ") // flatten newlines for single-line logs
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// handleMessage is the core pipeline — this is where every text message
// flows through. The spec's message flow (steps 1-11) happens here.
func (b *Bot) handleMessage(c tele.Context) error {
	msg := c.Message()
	userText := msg.Text

	// Get the active conversation ID for this chat.
	conversationID := b.getConversationID(msg.Chat.ID)

	log.Printf("─── incoming message ───")
	log.Printf("  <user> %s", truncate(userText, 100))

	// Step 3: Log the raw message to SQLite.
	msgID, err := b.store.SaveMessage("user", userText, "", conversationID)
	if err != nil {
		log.Printf("  ✗ error saving message: %v", err)
	}

	// Step 4: PII scrub the message.
	var scrubResult *scrub.ScrubResult
	if b.cfg.Scrub.Enabled {
		scrubResult = scrub.Scrub(userText)
		if vaultCount := len(scrubResult.Vault.Entries()); vaultCount > 0 {
			log.Printf("  scrub: %d PII token(s) replaced", vaultCount)
		}
	} else {
		scrubResult = &scrub.ScrubResult{
			Text:  userText,
			Vault: scrub.NewVault(),
		}
	}

	// Update the saved message with the scrubbed version.
	if msgID > 0 {
		b.store.UpdateMessageScrubbed(msgID, scrubResult.Text)
		for _, entry := range scrubResult.Vault.Entries() {
			if err := b.store.SavePIIVaultEntry(msgID, entry.Token, entry.Original, entry.EntityType); err != nil {
				log.Printf("  ✗ error saving PII vault entry: %v", err)
			}
		}
	}

	// Step 5: Retrieve recent conversation history for context.
	recentMsgs, err := b.store.RecentMessages(conversationID, b.cfg.Memory.RecentMessages)
	if err != nil {
		log.Printf("  ✗ error retrieving history: %v", err)
		recentMsgs = nil
	}
	log.Printf("  context: %d history messages", len(recentMsgs))

	// Step 6: Assemble the full prompt.
	llmMessages := b.buildPrompt(scrubResult.Text, recentMsgs)

	// Step 7: Send typing indicator.
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

	// Step 8: Call the LLM.
	start := time.Now()
	resp, err := b.llm.ChatCompletion(llmMessages)
	close(stopTyping)
	latencyMs := int(time.Since(start).Milliseconds())

	if err != nil {
		log.Printf("  ✗ LLM error: %v", err)
		return c.Send("Sorry, I'm having trouble thinking right now. Try again in a moment?")
	}

	log.Printf("  <mira> %s", truncate(resp.Content, 100))
	log.Printf("  tokens: %d prompt + %d completion = %d total | cost: $%.6f | latency: %dms",
		resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens, resp.CostUSD, latencyMs)

	// Step 9: Log the response to SQLite.
	respID, err := b.store.SaveMessage("assistant", resp.Content, resp.Content, conversationID)
	if err != nil {
		log.Printf("  ✗ error saving response: %v", err)
	}

	// Update token counts on both messages.
	if msgID > 0 {
		b.store.UpdateMessageTokenCount(msgID, resp.PromptTokens)
	}
	if respID > 0 {
		b.store.UpdateMessageTokenCount(respID, resp.CompletionTokens)
	}

	// Log metrics.
	if respID > 0 {
		b.store.SaveMetric(resp.Model, resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens, resp.CostUSD, latencyMs, respID)
	}

	// Step 10: Deanonymize and send reply.
	replyText := scrub.Deanonymize(resp.Content, scrubResult.Vault)
	if err := c.Send(replyText); err != nil {
		return err
	}

	log.Printf("  → reply sent, handing off to agent")

	// Step 11: Run the agent in a background goroutine.
	// Pass msgID so agent metrics link back to the triggering message.
	go b.runAgent(userText, resp.Content, msgID, c)

	return nil
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

	log.Printf("─── /clear ── conversation reset for chat %d → %s", chatID, newID)
	return c.Send("Context cleared. Fresh start!")
}

// handleStats shows aggregate usage statistics.
// Pulls data from the metrics and messages tables and formats it
// as a Telegram message with HTML formatting.
func (b *Bot) handleStats(c tele.Context) error {
	stats, err := b.store.GetStats()
	if err != nil {
		return c.Send("couldn't load stats right now, sorry!")
	}

	// Build a nicely formatted stats message.
	// We use HTML parse mode because it's easier to work with
	// programmatically than MarkdownV2 (no escape character hell).
	msg := fmt.Sprintf(
		"<b>📊 Stats</b>\n\n"+
			"<b>Messages:</b> %d total (%d you, %d me)\n"+
			"<b>Active days:</b> %d\n\n"+
			"<b>Memory:</b> %d facts (%d about you, %d about me)\n\n"+
			"<b>Tokens:</b> %s total\n"+
			"  Chat: %s ($%.4f)\n"+
			"  Agent: %s ($%.4f)\n"+
			"<b>Total cost:</b> $%.4f\n"+
			"<b>Avg latency:</b> %dms",
		stats.TotalMessages, stats.UserMessages, stats.MiraMessages,
		stats.ConversationDays,
		stats.TotalFacts, stats.UserFacts, stats.SelfFacts,
		formatTokens(stats.TotalTokens),
		formatTokens(stats.ChatTokens), stats.ChatCostUSD,
		formatTokens(stats.AgentTokens), stats.AgentCostUSD,
		stats.TotalCostUSD,
		stats.AvgLatencyMs,
	)

	return c.Send(msg, &tele.SendOptions{ParseMode: tele.ModeHTML})
}

// handleForget deactivates a fact by ID. Usage: /forget 3
// If no ID is given, lists all active facts so the user can pick one.
func (b *Bot) handleForget(c tele.Context) error {
	args := strings.TrimSpace(c.Message().Payload)

	// If no argument, show active facts so the user can pick.
	if args == "" {
		return b.handleFacts(c)
	}

	// Parse the fact ID.
	var factID int64
	if _, err := fmt.Sscanf(args, "%d", &factID); err != nil {
		return c.Send("usage: /forget <fact_id>\n\nUse /facts to see all active facts with their IDs.")
	}

	if err := b.store.DeactivateFact(factID); err != nil {
		return c.Send(fmt.Sprintf("couldn't forget fact %d: %v", factID, err))
	}

	log.Printf("─── /forget ── deactivated fact ID=%d", factID)
	return c.Send(fmt.Sprintf("Done — forgot fact #%d.", factID))
}

// handleFacts lists all active facts, grouped by subject.
// Useful for seeing what Mira knows and finding fact IDs for /forget.
func (b *Bot) handleFacts(c tele.Context) error {
	facts, err := b.store.AllActiveFacts()
	if err != nil {
		return c.Send("couldn't load facts right now, sorry!")
	}

	if len(facts) == 0 {
		return c.Send("No facts saved yet. Keep chatting!")
	}

	var msg strings.Builder
	msg.WriteString("<b>🧠 What I Know</b>\n\n")

	// Group by subject, show user facts first.
	currentSubject := ""
	for _, f := range facts {
		if f.Subject != currentSubject {
			currentSubject = f.Subject
			if currentSubject == "user" {
				msg.WriteString("<b>About you:</b>\n")
			} else {
				msg.WriteString("\n<b>About me:</b>\n")
			}
		}
		msg.WriteString(fmt.Sprintf("  #%d [%s, ★%d] %s\n", f.ID, f.Category, f.Importance, f.Fact))
	}

	msg.WriteString("\n<i>Use /forget &lt;id&gt; to remove a fact.</i>")

	return c.Send(msg.String(), &tele.SendOptions{ParseMode: tele.ModeHTML})
}

// formatTokens formats a token count with K/M suffixes for readability.
func formatTokens(n int) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.1fK", float64(n)/1_000)
	}
	return fmt.Sprintf("%d", n)
}

// runAgent kicks off the background agent (Liquid LFM) to process
// the latest exchange. The agent decides what memory operations to
// perform and can optionally send follow-up messages through Deepseek.
func (b *Bot) runAgent(userMessage, miraResponse string, triggerMsgID int64, c tele.Context) {
	// Build a send_message callback that routes through Deepseek.
	// When the agent calls send_message, we generate a response with
	// the conversational model and send it to Telegram.
	sendMsg := func(instruction string) error {
		// Build a minimal prompt for the follow-up.
		messages := []llm.ChatMessage{
			{Role: "system", Content: b.buildSystemPrompt()},
			{Role: "user", Content: instruction},
		}

		resp, err := b.llm.ChatCompletion(messages)
		if err != nil {
			return fmt.Errorf("generating follow-up: %w", err)
		}

		return c.Send(resp.Content)
	}

	agent.Run(b.agentLLM, b.store, b.embedClient, b.cfg.Embed.SimilarityThreshold, userMessage, miraResponse, b.cfg.Persona.PersonaFile, triggerMsgID, sendMsg)
}

// buildSystemPrompt assembles the full system prompt by reading prompt.md
// fresh from disk (hot-reloadable), then layering in persona.md and
// memory context (extracted facts).
func (b *Bot) buildSystemPrompt() string {
	var parts []string

	// Layer 1: prompt.md — base identity (hot-reloaded from disk).
	if promptBytes, err := os.ReadFile(b.cfg.Persona.PromptFile); err == nil {
		parts = append(parts, string(promptBytes))
	} else {
		parts = append(parts, b.systemPrompt)
	}

	// Layer 2: persona.md — evolving self-image (if it exists).
	if personaBytes, err := os.ReadFile(b.cfg.Persona.PersonaFile); err == nil {
		parts = append(parts, string(personaBytes))
	}

	// Layer 4: Memory context — extracted facts about the user.
	if memCtx, err := memory.BuildMemoryContext(b.store, b.cfg.Memory.MaxFactsInContext); err == nil && memCtx != "" {
		parts = append(parts, memCtx)
	}

	return strings.Join(parts, "\n\n---\n\n")
}

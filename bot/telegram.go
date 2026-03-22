// Package bot handles the Telegram interface — receiving messages,
// running them through the agent pipeline, and managing the UI.
package bot

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"her/agent"
	"her/compact"
	"her/config"
	"her/embed"
	"her/llm"
	"her/memory"
	"her/persona"
	"her/scrub"
	"her/search"

	tele "gopkg.in/telebot.v4"
)

// Bot wraps the Telegram bot and all its dependencies.
// This is a common Go pattern: a "god struct" that holds references
// to all the services a component needs. Similar to dependency injection
// in Python/Java, but done manually (Go favors explicitness over magic).
type Bot struct {
	tb           *tele.Bot
	llm          *llm.Client          // conversational model (Deepseek)
	agentLLM     *llm.Client          // tool-calling orchestrator
	embedClient  *embed.Client        // local embedding model for similarity
	tavilyClient *search.TavilyClient // web search and URL extraction
	store        *memory.Store
	cfg          *config.Config
	systemPrompt string
	startTime    time.Time

	// conversationIDs tracks the active conversation ID per chat.
	// When /clear is called, we rotate to a new ID so the history
	// window starts fresh.
	conversationIDs sync.Map
}

// New creates and configures a new Telegram bot.
func New(cfg *config.Config, llmClient *llm.Client, agentLLM *llm.Client, embedClient *embed.Client, tavilyClient *search.TavilyClient, store *memory.Store) (*Bot, error) {
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
		tavilyClient: tavilyClient,
		store:        store,
		cfg:          cfg,
		systemPrompt: string(promptBytes),
		startTime:    time.Now(),
	}

	// Register command handlers.
	tb.Handle("/clear", bot.handleClear)
	tb.Handle("/stats", bot.handleStats)
	tb.Handle("/forget", bot.handleForget)
	tb.Handle("/facts", bot.handleFacts)
	tb.Handle("/reflect", bot.handleReflect)
	tb.Handle("/persona", bot.handlePersona)
	tb.Handle("/compact", bot.handleCompact)
	tb.Handle("/status", bot.handleStatus)
	tb.Handle("/restart", bot.handleRestart)

	// Register message handler for all text messages.
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

// handleMessage is the core pipeline. In the new agent-first architecture:
//  1. Save & scrub the message
//  2. Send a placeholder Telegram message
//  3. Run the agent SYNCHRONOUSLY — it orchestrates searches, generates
//     the response via the reply tool, and manages memory
//  4. The placeholder message gets edited to show status updates and
//     the final response as tools execute
func (b *Bot) handleMessage(c tele.Context) error {
	msg := c.Message()
	userText := msg.Text

	// Get the active conversation ID for this chat.
	conversationID := b.getConversationID(msg.Chat.ID)

	log.Printf("--- incoming message ---")
	log.Printf("  <user> %s", truncate(userText, 100))

	// Step 1: Log the raw message to SQLite.
	msgID, err := b.store.SaveMessage("user", userText, "", conversationID)
	if err != nil {
		log.Printf("  error saving message: %v", err)
	}

	// Step 2: PII scrub the message.
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
				log.Printf("  error saving PII vault entry: %v", err)
			}
		}
	}

	// Step 3: Show typing indicator while we work.
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

	// Step 4: Send a placeholder message that we'll edit with status
	// updates as the agent works. The thinking emoji signals to the user
	// that we're processing their message.
	placeholder, sendErr := c.Bot().Send(c.Recipient(), "\U0001F4AD")
	if sendErr != nil {
		close(stopTyping)
		log.Printf("  error sending placeholder: %v", sendErr)
		return c.Send("Sorry, I'm having trouble right now. Try again in a moment?")
	}

	// Step 5: Build the status callback. This function gets passed to
	// the agent and is called whenever a tool wants to update the
	// Telegram message — search status indicators, the final reply, etc.
	//
	// In Python you'd pass a lambda: lambda status: bot.edit_message(msg_id, status)
	// In Go, closures work the same way — this function "closes over"
	// the placeholder variable so it always edits the right message.
	statusCallback := func(status string) error {
		_, err := c.Bot().Edit(placeholder, status)
		return err
	}

	// Step 6: Run the agent SYNCHRONOUSLY. The agent is the pipeline now —
	// it decides whether to search, what to reply, and handles memory.
	// This blocks until the agent has called reply and finished.
	result, err := agent.Run(agent.RunParams{
		AgentLLM:            b.agentLLM,
		ChatLLM:             b.llm,
		Store:               b.store,
		EmbedClient:         b.embedClient,
		SimilarityThreshold: b.cfg.Embed.SimilarityThreshold,
		TavilyClient:        b.tavilyClient,
		Cfg:                 b.cfg,
		ScrubbedUserMessage: scrubResult.Text,
		ScrubVault:          scrubResult.Vault,
		ConversationID:      conversationID,
		TriggerMsgID:        msgID,
		StatusCallback:      statusCallback,
		ReflectionThreshold: b.cfg.Persona.ReflectionMemoryThreshold,
		RewriteEveryN:       b.cfg.Persona.RewriteEveryNConversations,
	})

	close(stopTyping)

	if err != nil {
		log.Printf("  agent error: %v", err)
		// If the agent failed entirely, edit the placeholder with an error message.
		_, _ = c.Bot().Edit(placeholder, "Sorry, I'm having trouble thinking right now. Try again in a moment?")
		return nil
	}

	log.Printf("  <mira> %s", truncate(result.ReplyText, 100))
	log.Printf("  -> reply sent via agent pipeline")

	return nil
}

// getConversationID returns the active conversation ID for a chat.
// On first call after a restart, it checks the database for the most
// recent conversation ID for this chat, so the bot resumes where it
// left off instead of starting a new conversation and losing context.
func (b *Bot) getConversationID(chatID int64) string {
	key := fmt.Sprintf("%d", chatID)

	// Check in-memory cache first.
	if val, ok := b.conversationIDs.Load(key); ok {
		return val.(string)
	}

	// Not in memory (first message after restart). Check the DB
	// for the most recent conversation with this chat.
	prefix := fmt.Sprintf("tg_%d", chatID)
	if existing := b.store.LatestConversationID(prefix); existing != "" {
		b.conversationIDs.Store(key, existing)
		log.Printf("  [bot] resumed conversation: %s", existing)
		return existing
	}

	// No existing conversation. Create a new one.
	newID := fmt.Sprintf("tg_%d_%d", chatID, time.Now().Unix())
	b.conversationIDs.Store(key, newID)
	return newID
}

// handleClear resets the conversation context.
func (b *Bot) handleClear(c tele.Context) error {
	chatID := c.Message().Chat.ID
	key := fmt.Sprintf("%d", chatID)

	newID := fmt.Sprintf("tg_%d_%d", chatID, time.Now().Unix())
	b.conversationIDs.Store(key, newID)

	log.Printf("--- /clear -- conversation reset for chat %d -> %s", chatID, newID)
	return c.Send("Context cleared. Fresh start!")
}

// handleStats shows aggregate usage statistics.
func (b *Bot) handleStats(c tele.Context) error {
	stats, err := b.store.GetStats()
	if err != nil {
		return c.Send("couldn't load stats right now, sorry!")
	}

	msg := fmt.Sprintf(
		"<b>\U0001F4CA Stats</b>\n\n"+
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

// handleForget deactivates a fact by ID.
func (b *Bot) handleForget(c tele.Context) error {
	args := strings.TrimSpace(c.Message().Payload)

	if args == "" {
		return b.handleFacts(c)
	}

	var factID int64
	if _, err := fmt.Sscanf(args, "%d", &factID); err != nil {
		return c.Send("usage: /forget <fact_id>\n\nUse /facts to see all active facts with their IDs.")
	}

	if err := b.store.DeactivateFact(factID); err != nil {
		return c.Send(fmt.Sprintf("couldn't forget fact %d: %v", factID, err))
	}

	log.Printf("--- /forget -- deactivated fact ID=%d", factID)
	return c.Send(fmt.Sprintf("Done — forgot fact #%d.", factID))
}

// handleFacts lists all active facts, grouped by subject.
func (b *Bot) handleFacts(c tele.Context) error {
	facts, err := b.store.AllActiveFacts()
	if err != nil {
		return c.Send("couldn't load facts right now, sorry!")
	}

	if len(facts) == 0 {
		return c.Send("No facts saved yet. Keep chatting!")
	}

	var msg strings.Builder
	msg.WriteString("<b>\U0001F9E0 What I Know</b>\n\n")

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
		msg.WriteString(fmt.Sprintf("  #%d [%s, \u2605%d] %s\n", f.ID, f.Category, f.Importance, f.Fact))
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

// handleReflect manually triggers a reflection.
func (b *Bot) handleReflect(c tele.Context) error {
	_ = c.Notify(tele.Typing)

	recent, err := b.store.GlobalRecentMessages(10)
	if err != nil || len(recent) < 2 {
		return c.Send("Not enough conversation history to reflect on yet. Keep chatting!")
	}

	facts, _ := b.store.RecentFacts("user", 10)
	selfFacts, _ := b.store.RecentFacts("self", 10)

	var factStrings []string
	for _, f := range facts {
		factStrings = append(factStrings, f.Fact)
	}
	for _, f := range selfFacts {
		if f.Category != "reflection" {
			factStrings = append(factStrings, "(self) "+f.Fact)
		}
	}

	if len(factStrings) == 0 {
		return c.Send("I don't have enough memories to reflect on yet. Let's keep talking!")
	}

	var lastUser, lastMira string
	for i := len(recent) - 1; i >= 0; i-- {
		if recent[i].Role == "user" && lastUser == "" {
			lastUser = recent[i].ContentRaw
		}
		if recent[i].Role == "assistant" && lastMira == "" {
			lastMira = recent[i].ContentRaw
		}
		if lastUser != "" && lastMira != "" {
			break
		}
	}

	err = persona.Reflect(b.llm, b.store, lastUser, lastMira, factStrings)
	if err != nil {
		log.Printf("  manual reflection error: %v", err)
		return c.Send("I tried to reflect but something went wrong. Try again?")
	}

	reflections, _ := b.store.ReflectionsSince(time.Now().Add(-10 * time.Second))
	if len(reflections) > 0 {
		return c.Send(fmt.Sprintf("\U0001F4AD <b>Reflection</b>\n\n<i>%s</i>", reflections[len(reflections)-1].Fact),
			&tele.SendOptions{ParseMode: tele.ModeHTML})
	}

	return c.Send("Done reflecting. Use /facts to see what I wrote.")
}

// handlePersona shows the current persona.md content.
func (b *Bot) handlePersona(c tele.Context) error {
	args := strings.TrimSpace(c.Message().Payload)

	if args == "history" {
		return b.handlePersonaHistory(c)
	}

	data, err := os.ReadFile(b.cfg.Persona.PersonaFile)
	if err != nil || len(data) == 0 {
		return c.Send("No persona description yet. I'll develop one as we keep chatting!")
	}

	msg := fmt.Sprintf("\U0001FA9E <b>Who I Am Right Now</b>\n\n<i>%s</i>", string(data))
	return c.Send(msg, &tele.SendOptions{ParseMode: tele.ModeHTML})
}

// handlePersonaHistory shows past persona versions.
func (b *Bot) handlePersonaHistory(c tele.Context) error {
	versions, err := b.store.PersonaHistory(5)
	if err != nil || len(versions) == 0 {
		return c.Send("No persona history yet. My personality hasn't been rewritten yet!")
	}

	var msg strings.Builder
	msg.WriteString("\U0001FA9E <b>Persona History</b>\n\n")
	for _, v := range versions {
		msg.WriteString(fmt.Sprintf("<b>v%d</b> \u2014 %s\n<i>Trigger: %s</i>\n",
			v.ID, v.Timestamp.Format("Jan 2, 3:04 PM"), v.Trigger))
		content := v.Content
		if len(content) > 150 {
			content = content[:150] + "..."
		}
		msg.WriteString(fmt.Sprintf("<code>%s</code>\n\n", content))
	}

	return c.Send(msg.String(), &tele.SendOptions{ParseMode: tele.ModeHTML})
}

// handleCompact manually triggers conversation compaction.
func (b *Bot) handleCompact(c tele.Context) error {
	convID := b.getConversationID(c.Message().Chat.ID)
	recent, err := b.store.RecentMessages(convID, b.cfg.Memory.RecentMessages)
	if err != nil || len(recent) < 4 {
		return c.Send("Not enough messages to compact yet.")
	}

	tokensBefore := compact.EstimateHistoryTokens("", recent)

	// Force compaction by passing a very low threshold (0 = always compact).
	summary, kept, err := compact.MaybeCompact(b.llm, b.store, convID, recent, 1)
	if err != nil {
		return c.Send(fmt.Sprintf("Compaction failed: %v", err))
	}

	tokensAfter := compact.EstimateHistoryTokens(summary, kept)
	saved := tokensBefore - tokensAfter

	msg := fmt.Sprintf(
		"\U0001F5DC <b>Compacted</b>\n\n"+
			"Messages: %d \u2192 %d kept\n"+
			"Tokens: ~%d \u2192 ~%d (saved ~%d)\n\n"+
			"<i>Summary:</i>\n%s",
		len(recent), len(kept),
		tokensBefore, tokensAfter, saved,
		summary,
	)
	return c.Send(msg, &tele.SendOptions{ParseMode: tele.ModeHTML})
}

// handleStatus shows the bot's current operational state.
func (b *Bot) handleStatus(c tele.Context) error {
	uptime := time.Since(b.startTime).Round(time.Second)
	convID := b.getConversationID(c.Message().Chat.ID)

	stats, _ := b.store.GetStats()

	// Check which services are available.
	embedStatus := "off"
	if b.embedClient != nil {
		embedStatus = "on"
	}
	tavilyStatus := "off"
	if b.tavilyClient != nil {
		tavilyStatus = "on"
	}

	// Check if running under launchd.
	managedBy := "manual (go run)"
	if os.Getenv("__CFBundleIdentifier") != "" || isLaunchdManaged() {
		managedBy = "launchd"
	}

	msg := fmt.Sprintf(
		"\U0001F4DF <b>Status</b>\n\n"+
			"<b>Uptime:</b> %s\n"+
			"<b>Process:</b> %s\n"+
			"<b>Go:</b> %s\n"+
			"<b>Conversation:</b> %s\n\n"+
			"<b>Models:</b>\n"+
			"  Chat: %s\n"+
			"  Agent: %s\n\n"+
			"<b>Services:</b>\n"+
			"  Embeddings: %s\n"+
			"  Web search: %s\n\n"+
			"<b>Session:</b>\n"+
			"  Messages: %d\n"+
			"  Facts: %d\n"+
			"  Cost: $%.4f",
		uptime, managedBy, runtime.Version(), convID,
		b.cfg.LLM.Model, b.cfg.Agent.Model,
		embedStatus, tavilyStatus,
		stats.TotalMessages, stats.TotalFacts, stats.TotalCostUSD,
	)
	return c.Send(msg, &tele.SendOptions{ParseMode: tele.ModeHTML})
}

// handleRestart restarts the bot process. If running under launchd,
// uses launchctl to do a clean restart. Otherwise, exits and relies
// on the user to restart manually.
func (b *Bot) handleRestart(c tele.Context) error {
	log.Printf("--- /restart -- restart requested via Telegram")

	if isLaunchdManaged() {
		_ = c.Send("Restarting via launchd... be right back.")

		// launchctl kickstart -k forces a restart of the service.
		// The -k flag kills the existing instance first.
		go func() {
			time.Sleep(500 * time.Millisecond) // let the message send
			cmd := exec.Command("launchctl", "kickstart", "-k", "gui/"+fmt.Sprintf("%d", os.Getuid())+"/com.mira.her-go")
			if err := cmd.Run(); err != nil {
				log.Printf("  launchctl kickstart failed: %v, falling back to exit", err)
				os.Exit(0) // launchd will restart us via KeepAlive
			}
		}()
		return nil
	}

	// Not managed by launchd. Just exit cleanly.
	_ = c.Send("Shutting down. Restart me manually with `go run main.go`.")
	go func() {
		time.Sleep(500 * time.Millisecond)
		b.Stop()
	}()
	return nil
}

// isLaunchdManaged checks if the bot is running as a launchd service
// by looking for the service in launchctl.
func isLaunchdManaged() bool {
	cmd := exec.Command("launchctl", "print", "gui/"+fmt.Sprintf("%d", os.Getuid())+"/com.mira.her-go")
	return cmd.Run() == nil
}

// buildSystemPrompt assembles the full system prompt by reading prompt.md
// fresh from disk (hot-reloadable), then layering in persona.md and
// memory context (extracted facts).
//
// This is still used by /reflect which calls the conversational model
// directly. The main message pipeline now uses the agent's buildChatSystemPrompt.
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

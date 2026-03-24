// Package bot handles the Telegram interface — receiving messages,
// running them through the agent pipeline, and managing the UI.
package bot

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"her/agent"
	"her/compact"
	"her/config"
	"her/embed"
	"her/logger"
	"her/llm"
	"her/memory"
	"her/persona"
	"her/scrub"
	"her/search"
	"her/voice"
	"her/weather"

	tele "gopkg.in/telebot.v4"
)

// log is the package-level logger for the bot package.
var log = logger.WithPrefix("bot")

// Bot wraps the Telegram bot and all its dependencies.
// This is a common Go pattern: a "god struct" that holds references
// to all the services a component needs. Similar to dependency injection
// in Python/Java, but done manually (Go favors explicitness over magic).
type Bot struct {
	tb           *tele.Bot
	llm          *llm.Client          // conversational model (Deepseek)
	agentLLM     *llm.Client          // tool-calling orchestrator
	visionLLM    *llm.Client          // vision language model (Gemini Flash) — nil if not configured
	embedClient  *embed.Client        // local embedding model for similarity
	tavilyClient  *search.TavilyClient  // web search and URL extraction
	weatherClient *weather.Client      // Open-Meteo weather — nil if not configured
	voiceClient   *voice.Client        // local STT via parakeet-server — nil if voice disabled
	ttsClient    *voice.TTSClient    // local TTS via kokoro/mlx-audio — nil if TTS disabled
	store        *memory.Store
	cfg          *config.Config
	configPath   string               // path to config.yaml — needed for /traces toggle
	systemPrompt string
	startTime    time.Time

	// conversationIDs tracks the active conversation ID per chat.
	// When /clear is called, we rotate to a new ID so the history
	// window starts fresh.
	conversationIDs sync.Map
}

// New creates and configures a new Telegram bot.
func New(cfg *config.Config, configPath string, llmClient *llm.Client, agentLLM *llm.Client, visionLLM *llm.Client, embedClient *embed.Client, tavilyClient *search.TavilyClient, weatherClient *weather.Client, voiceClient *voice.Client, ttsClient *voice.TTSClient, store *memory.Store) (*Bot, error) {
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
		tb:            tb,
		llm:           llmClient,
		agentLLM:      agentLLM,
		visionLLM:     visionLLM,
		embedClient:   embedClient,
		tavilyClient:  tavilyClient,
		weatherClient: weatherClient,
		voiceClient:   voiceClient,
		ttsClient:     ttsClient,
		store:         store,
		cfg:           cfg,
		configPath:    configPath,
		systemPrompt:  string(promptBytes),
		startTime:     time.Now(),
	}

	// Register command handlers.
	tb.Handle("/help", bot.handleHelp)
	tb.Handle("/clear", bot.handleClear)
	tb.Handle("/stats", bot.handleStats)
	tb.Handle("/forget", bot.handleForget)
	tb.Handle("/facts", bot.handleFacts)
	tb.Handle("/reflect", bot.handleReflect)
	tb.Handle("/persona", bot.handlePersona)
	tb.Handle("/compact", bot.handleCompact)
	tb.Handle("/status", bot.handleStatus)
	tb.Handle("/restart", bot.handleRestart)
	tb.Handle("/remind", bot.handleRemind)
	tb.Handle("/schedule", bot.handleSchedule)
	tb.Handle("/traces", bot.handleTraces)

	// Register message handler for all text messages.
	tb.Handle(tele.OnText, bot.handleMessage)

	// Register photo handler for image understanding (v0.2.5).
	// In telebot, tele.OnPhoto fires when a user sends an image.
	// Photos can optionally have a caption (text alongside the image).
	tb.Handle(tele.OnPhoto, bot.handlePhoto)

	// Register voice handler for speech-to-text (v0.3).
	// tele.OnVoice fires when a user sends a voice memo (the
	// microphone button in Telegram). Audio files sent as documents
	// use tele.OnDocument instead — we only handle voice memos here.
	tb.Handle(tele.OnVoice, bot.handleVoice)

	// Register inline keyboard callback handlers (v0.6).
	// Each Action value in scheduler.Button needs a handler here.
	// See bot/callbacks.go for the implementations.
	bot.registerCallbackHandlers()

	return bot, nil
}

// Start begins polling Telegram for messages. This blocks forever
// (or until the bot is stopped), so it's typically the last thing
// called in main.go.
func (b *Bot) Start() {
	log.Info("Bot is running. Listening for messages...")
	b.tb.Start()
}

// Stop gracefully shuts down the bot.
func (b *Bot) Stop() {
	b.tb.Stop()
}

// chatRecipient implements tele.Recipient for sending to a specific chat ID.
// In Go, interfaces are satisfied implicitly — any type that has a
// Recipient() string method satisfies tele.Recipient. No "implements"
// keyword needed. This is like Python's duck typing but checked at
// compile time.
type chatRecipient struct {
	chatID string
}

func (r chatRecipient) Recipient() string { return r.chatID }

// SendToChat sends a text message to a specific Telegram chat.
// Used by the scheduler to deliver reminders — it doesn't have a
// tele.Context, so it calls this directly with the chat ID.
func (b *Bot) SendToChat(chatID int64, text string) error {
	_, err := b.tb.Send(
		chatRecipient{chatID: fmt.Sprintf("%d", chatID)},
		text,
		&tele.SendOptions{ParseMode: tele.ModeHTML},
	)
	return err
}

// truncate shortens a string for log output, adding "..." if it was cut.
func truncate(s string, maxLen int) string {
	s = strings.ReplaceAll(s, "\n", " ") // flatten newlines for single-line logs
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// stripHTML removes HTML tags for a plain-text fallback when Telegram's
// HTML parser rejects our formatting. Crude but effective for traces.
func stripHTML(s string) string {
	s = strings.ReplaceAll(s, "<b>", "")
	s = strings.ReplaceAll(s, "</b>", "")
	s = strings.ReplaceAll(s, "<i>", "")
	s = strings.ReplaceAll(s, "</i>", "")
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	return s
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

	log.Info("─── incoming message ───")
	log.Infof("  user: %s", truncate(userText, 100))

	// Step 1: Log the raw message to SQLite.
	msgID, err := b.store.SaveMessage("user", userText, "", conversationID)
	if err != nil {
		log.Error("saving message", "err", err)
	}

	// Step 2: PII scrub the message.
	var scrubResult *scrub.ScrubResult
	if b.cfg.Scrub.Enabled {
		scrubResult = scrub.Scrub(userText)
		if vaultCount := len(scrubResult.Vault.Entries()); vaultCount > 0 {
			log.Info("PII scrubbed", "tokens", vaultCount)
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
				log.Error("saving PII vault entry", "err", err)
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

	// Step 4: Build the trace callback FIRST if enabled — its placeholder
	// (🧠) needs to appear ABOVE the reply placeholder in chat order.
	var traceCallback agent.TraceCallback
	if b.cfg.Agent.Trace {
		traceCallback = b.makeTraceCallback(c)
	}

	// Step 5: Send the reply placeholder message that we'll edit with
	// the final response. The thinking emoji signals to the user that
	// we're processing their message.
	placeholder, sendErr := c.Bot().Send(c.Recipient(), "\U0001F4AD")
	if sendErr != nil {
		close(stopTyping)
		log.Error("sending placeholder", "err", sendErr)
		return c.Send("Sorry, I'm having trouble right now. Try again in a moment?")
	}

	// Build the status callback — edits the placeholder with the final
	// reply text (or intermediate status updates like "searching...").
	statusCallback := func(status string) error {
		_, err := c.Bot().Edit(placeholder, status)
		return err
	}

	// sendCallback sends a NEW message (rather than editing the placeholder).
	// Used by the reply tool for follow-up replies — e.g., after "let me
	// look that up", the actual answer comes as a separate message.
	sendCallback := func(text string) error {
		_, err := c.Bot().Send(c.Recipient(), text, &tele.SendOptions{ParseMode: tele.ModeHTML})
		return err
	}

	// Build the TTS callback — fires inside execReply so voice synthesis
	// starts immediately when text is sent, not after the whole agent loop.
	var ttsCallback agent.TTSCallback
	if b.ttsClient != nil && b.ttsClient.ReplyMode() == "voice" {
		ttsCallback = func(text string) {
			b.sendVoiceReply(c, text)
		}
	}

	result, err := agent.Run(agent.RunParams{
		AgentLLM:            b.agentLLM,
		ChatLLM:             b.llm,
		VisionLLM:           b.visionLLM,
		Store:               b.store,
		EmbedClient:         b.embedClient,
		SimilarityThreshold: b.cfg.Embed.SimilarityThreshold,
		TavilyClient:        b.tavilyClient,
		WeatherClient:       b.weatherClient,
		Cfg:                 b.cfg,
		ScrubbedUserMessage: scrubResult.Text,
		ScrubVault:          scrubResult.Vault,
		ConversationID:      conversationID,
		TriggerMsgID:        msgID,
		StatusCallback:      statusCallback,
		SendCallback:        sendCallback,
		TTSCallback:         ttsCallback,
		TraceCallback:       traceCallback,
		ReflectionThreshold: b.cfg.Persona.ReflectionMemoryThreshold,
		RewriteEveryN:       b.cfg.Persona.RewriteEveryNConversations,
	})

	close(stopTyping)

	if err != nil {
		log.Error("agent error", "err", err)
		_, _ = c.Bot().Edit(placeholder, "Sorry, I'm having trouble thinking right now. Try again in a moment?")
		return nil
	}

	log.Infof("  mira: %s", truncate(result.ReplyText, 100))

	log.Info("─── reply sent ───")

	return nil
}

// handlePhoto processes incoming photos with optional captions.
// This is the entry point for v0.2.5 "She Sees" — image understanding.
//
// The flow is almost identical to handleMessage, but with extra steps:
//  1. Download the image from Telegram's servers
//  2. Base64-encode it (the format VLMs expect for inline images)
//  3. Detect the MIME type (jpeg, png, etc.)
//  4. Pass the image data + caption through the agent pipeline
//
// In telebot v4, tele.OnPhoto gives us c.Message().Photo which is a
// single *tele.Photo (the highest-quality version Telegram selected).
// The photo's FileID lets us download the actual bytes.
func (b *Bot) handlePhoto(c tele.Context) error {
	msg := c.Message()
	photo := msg.Photo

	if photo == nil {
		return c.Send("I couldn't read that photo. Try sending it again?")
	}

	// Captions on photos live in msg.Caption, not msg.Text.
	// This catches people who send a photo with a question like
	// "what is this?" written underneath it.
	caption := msg.Caption
	conversationID := b.getConversationID(msg.Chat.ID)

	log.Info("─── incoming photo ───")
	if caption != "" {
		log.Infof("  caption: %s", truncate(caption, 100))
	}

	// Step 1: Download the image from Telegram's servers.
	// telebot's File method returns a ReadCloser — same idea as Python's
	// response = requests.get(url), but you get a stream instead of
	// the full body. We read all bytes with io.ReadAll (like response.content).
	reader, err := c.Bot().File(&photo.File)
	if err != nil {
		log.Error("downloading photo", "err", err)
		return c.Send("I couldn't download that photo. Try again?")
	}
	defer reader.Close()

	imageBytes, err := io.ReadAll(reader)
	if err != nil {
		log.Error("reading photo bytes", "err", err)
		return c.Send("I couldn't read that photo. Try again?")
	}

	// Step 2: Base64-encode the image.
	// base64.StdEncoding.EncodeToString(data) is Go's equivalent of
	// Python's base64.b64encode(data).decode('utf-8').
	// The VLM expects this format inside a data: URI.
	imageBase64 := base64.StdEncoding.EncodeToString(imageBytes)

	// Step 3: Detect MIME type from the file bytes.
	// http.DetectContentType reads the first 512 bytes and sniffs the
	// format — like Python's magic library but built into the stdlib.
	// Telegram usually sends JPEG, but users might send PNG or WebP.
	imageMIME := http.DetectContentType(imageBytes)

	log.Infof("  photo: %dx%d, %s, %d bytes", photo.Width, photo.Height, imageMIME, len(imageBytes))

	// Step 4: Build the user message text.
	// The agent sees this as the "user said" content. The image itself
	// travels separately via RunParams.ImageBase64.
	userText := "[User sent a photo]"
	if caption != "" {
		userText = "[User sent a photo] " + caption
	}

	// From here, same pipeline as handleMessage: save, scrub, type, run agent.
	msgID, err := b.store.SaveMessage("user", userText, "", conversationID)
	if err != nil {
		log.Error("saving message", "err", err)
	}

	// Store the Telegram file ID so we can re-download the image later.
	// The file_id is stable — Telegram keeps it around and you can fetch
	// the file bytes again with bot.File(&tele.File{FileID: ...}).
	if msgID > 0 {
		if err := b.store.UpdateMessageMedia(msgID, photo.FileID, ""); err != nil {
			log.Error("saving media file_id", "err", err)
		}
	}

	// PII scrub the caption (not the image — images aren't text).
	var scrubResult *scrub.ScrubResult
	if b.cfg.Scrub.Enabled {
		scrubResult = scrub.Scrub(userText)
		if vaultCount := len(scrubResult.Vault.Entries()); vaultCount > 0 {
			log.Info("PII scrubbed", "tokens", vaultCount)
		}
	} else {
		scrubResult = &scrub.ScrubResult{
			Text:  userText,
			Vault: scrub.NewVault(),
		}
	}

	if msgID > 0 {
		b.store.UpdateMessageScrubbed(msgID, scrubResult.Text)
		for _, entry := range scrubResult.Vault.Entries() {
			if err := b.store.SavePIIVaultEntry(msgID, entry.Token, entry.Original, entry.EntityType); err != nil {
				log.Error("saving PII vault entry", "err", err)
			}
		}
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

	// Trace placeholder first (so it appears above the reply).
	var traceCallback agent.TraceCallback
	if b.cfg.Agent.Trace {
		traceCallback = b.makeTraceCallback(c)
	}

	// Reply placeholder message.
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

	// Run the agent with image data attached.
	result, err := agent.Run(agent.RunParams{
		AgentLLM:            b.agentLLM,
		ChatLLM:             b.llm,
		VisionLLM:           b.visionLLM,
		Store:               b.store,
		EmbedClient:         b.embedClient,
		SimilarityThreshold: b.cfg.Embed.SimilarityThreshold,
		TavilyClient:        b.tavilyClient,
		WeatherClient:       b.weatherClient,
		Cfg:                 b.cfg,
		ScrubbedUserMessage: scrubResult.Text,
		ScrubVault:          scrubResult.Vault,
		ConversationID:      conversationID,
		TriggerMsgID:        msgID,
		StatusCallback:      statusCallback,
		SendCallback:        sendCallback,
		TraceCallback:       traceCallback,
		ReflectionThreshold: b.cfg.Persona.ReflectionMemoryThreshold,
		RewriteEveryN:       b.cfg.Persona.RewriteEveryNConversations,
		ImageBase64:         imageBase64,
		ImageMIME:           imageMIME,
	})

	close(stopTyping)

	if err != nil {
		log.Error("agent error", "err", err)
		_, _ = c.Bot().Edit(placeholder, "Sorry, I'm having trouble thinking right now. Try again in a moment?")
		return nil
	}

	log.Infof("  mira: %s", truncate(result.ReplyText, 100))
	log.Info("─── reply sent ───")

	return nil
}

// handleVoice processes incoming voice memos (v0.3 — "She Listens").
//
// Flow:
//  1. Check that the voice client is available
//  2. Download the .ogg file from Telegram
//  3. Save audio to a local file for the record
//  4. Send to parakeet-server for transcription
//  5. Feed transcribed text into the normal agent pipeline
//  6. Store voice_memo_path and file_id in the database
//
// Telegram sends voice memos as Ogg/Opus files. The parakeet-server
// handles format conversion internally (via ffmpeg), so we just forward
// the raw bytes.
func (b *Bot) handleVoice(c tele.Context) error {
	msg := c.Message()
	v := msg.Voice

	if v == nil {
		return c.Send("I couldn't read that voice memo. Try again?")
	}

	// Check if voice/STT is configured and available.
	if b.voiceClient == nil {
		return c.Send("Voice memos aren't enabled right now. Send me a text message instead?")
	}

	conversationID := b.getConversationID(msg.Chat.ID)

	log.Info("─── incoming voice memo ───")
	log.Infof("  duration: %ds, mime: %s", v.Duration, v.MIME)

	// Step 1: Download the audio from Telegram.
	// Same pattern as handlePhoto — telebot gives us a ReadCloser
	// via bot.File(), we read all bytes with io.ReadAll.
	reader, err := c.Bot().File(&v.File)
	if err != nil {
		log.Error("downloading voice memo", "err", err)
		return c.Send("I couldn't download that voice memo. Try again?")
	}
	defer reader.Close()

	audioBytes, err := io.ReadAll(reader)
	if err != nil {
		log.Error("reading voice bytes", "err", err)
		return c.Send("I couldn't read that voice memo. Try again?")
	}

	log.Infof("  downloaded: %d bytes", len(audioBytes))

	// Step 2: Save the audio locally so we have a record.
	// Files go to voice_memos/<conversation_id>/<timestamp>.ogg
	voiceDir := fmt.Sprintf("voice_memos/%s", conversationID)
	if err := os.MkdirAll(voiceDir, 0o755); err != nil {
		log.Error("creating voice memo directory", "err", err)
	}
	voicePath := fmt.Sprintf("%s/%d.ogg", voiceDir, msg.Unixtime)
	if err := os.WriteFile(voicePath, audioBytes, 0o644); err != nil {
		log.Error("saving voice memo file", "err", err)
		voicePath = "" // non-fatal — continue with transcription
	} else {
		log.Infof("  saved: %s", voicePath)
	}

	// Step 3: Start the typing indicator BEFORE transcription.
	// Transcription can take 5-15 seconds, so we need continuous
	// typing feedback the entire time. Telegram's typing indicator
	// expires after ~5 seconds, so we refresh it every 4.
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

	// Step 4: Transcribe via parakeet-server.
	transcript, err := b.voiceClient.Transcribe(audioBytes, "voice.ogg")
	if err != nil {
		close(stopTyping)
		log.Error("transcription failed", "err", err)
		return c.Send("I couldn't transcribe that voice memo. Is the speech server running? (parakeet-server)")
	}

	if transcript == "" {
		close(stopTyping)
		log.Warn("empty transcription")
		return c.Send("I couldn't make out what you said. Could you try again?")
	}

	log.Infof("  transcript: %s", truncate(transcript, 100))

	// Trace placeholder first (so it appears above the reply) — but only
	// for voice handler, since the main trace callback is built later.
	// We build it here just to send the 🧠 placeholder in the right order.
	var traceCallback agent.TraceCallback
	if b.cfg.Agent.Trace {
		traceCallback = b.makeTraceCallback(c)
	}

	// Step 5: Send a placeholder showing the transcript so the user
	// can see what was heard while the bot thinks about a response.
	placeholderText := fmt.Sprintf("\U0001F3A4 <i>%s</i>\n\n\U0001F4AD", transcript)
	placeholder, sendErr := c.Bot().Send(c.Recipient(), placeholderText, &tele.SendOptions{ParseMode: tele.ModeHTML})
	if sendErr != nil {
		close(stopTyping)
		log.Error("sending placeholder", "err", sendErr)
		return c.Send("Sorry, I'm having trouble right now. Try again in a moment?")
	}

	// Step 6: From here, same pipeline as handleMessage.
	// The transcribed text IS the user message — we treat it exactly
	// like they typed it, except we also store the voice memo path.
	userText := transcript

	// Save to DB with the transcribed text as content.
	msgID, err := b.store.SaveMessage("user", userText, "", conversationID)
	if err != nil {
		log.Error("saving message", "err", err)
	}

	// Store the voice memo path and Telegram file_id.
	if msgID > 0 {
		if voicePath != "" {
			if err := b.store.UpdateMessageVoicePath(msgID, voicePath); err != nil {
				log.Error("saving voice memo path", "err", err)
			}
		}
		if err := b.store.UpdateMessageMedia(msgID, v.FileID, ""); err != nil {
			log.Error("saving voice file_id", "err", err)
		}
	}

	// PII scrub the transcribed text.
	var scrubResult *scrub.ScrubResult
	if b.cfg.Scrub.Enabled {
		scrubResult = scrub.Scrub(userText)
		if vaultCount := len(scrubResult.Vault.Entries()); vaultCount > 0 {
			log.Info("PII scrubbed", "tokens", vaultCount)
		}
	} else {
		scrubResult = &scrub.ScrubResult{
			Text:  userText,
			Vault: scrub.NewVault(),
		}
	}

	if msgID > 0 {
		b.store.UpdateMessageScrubbed(msgID, scrubResult.Text)
		for _, entry := range scrubResult.Vault.Entries() {
			if err := b.store.SavePIIVaultEntry(msgID, entry.Token, entry.Original, entry.EntityType); err != nil {
				log.Error("saving PII vault entry", "err", err)
			}
		}
	}

	statusCallback := func(status string) error {
		_, err := c.Bot().Edit(placeholder, status, &tele.SendOptions{ParseMode: tele.ModeHTML})
		return err
	}
	sendCallback := func(text string) error {
		_, err := c.Bot().Send(c.Recipient(), text, &tele.SendOptions{ParseMode: tele.ModeHTML})
		return err
	}

	// TTS callback — same pattern as handleMessage.
	var ttsCallback agent.TTSCallback
	if b.ttsClient != nil {
		ttsCallback = func(text string) {
			b.sendVoiceReply(c, text)
		}
	}

	// Run the agent pipeline with the transcribed text.
	result, err := agent.Run(agent.RunParams{
		AgentLLM:            b.agentLLM,
		ChatLLM:             b.llm,
		VisionLLM:           b.visionLLM,
		Store:               b.store,
		EmbedClient:         b.embedClient,
		SimilarityThreshold: b.cfg.Embed.SimilarityThreshold,
		TavilyClient:        b.tavilyClient,
		WeatherClient:       b.weatherClient,
		Cfg:                 b.cfg,
		ScrubbedUserMessage: scrubResult.Text,
		ScrubVault:          scrubResult.Vault,
		ConversationID:      conversationID,
		TriggerMsgID:        msgID,
		StatusCallback:      statusCallback,
		SendCallback:        sendCallback,
		TTSCallback:         ttsCallback,
		TraceCallback:       traceCallback,
		ReflectionThreshold: b.cfg.Persona.ReflectionMemoryThreshold,
		RewriteEveryN:       b.cfg.Persona.RewriteEveryNConversations,
	})

	close(stopTyping)

	if err != nil {
		log.Error("agent error", "err", err)
		_, _ = c.Bot().Edit(placeholder, "Sorry, I'm having trouble thinking right now. Try again in a moment?")
		return nil
	}

	log.Infof("  mira: %s", truncate(result.ReplyText, 100))

	log.Info("─── voice reply sent ───")

	return nil
}

// sendVoiceReply synthesizes text to speech and sends it as a Telegram
// voice memo. Called from handleVoice (and potentially handleMessage
// when reply_mode is "voice").
//
// Telegram voice memos require Opus-encoded audio in an OGG container.
// The TTS client handles the WAV→OGG/Opus conversion via ffmpeg.
func (b *Bot) sendVoiceReply(c tele.Context, text string) {
	oggBytes, err := b.ttsClient.Synthesize(text)
	if err != nil {
		log.Error("TTS synthesis failed", "err", err)
		return
	}

	// In telebot, sending a voice memo requires a tele.Voice with a
	// File that has a Reader. We wrap the OGG bytes in a bytes.Reader
	// and set the MIME type so Telegram knows it's audio.
	//
	// tele.FromReader creates a File from an io.Reader — similar to how
	// handlePhoto uses bot.File() but in reverse (sending instead of receiving).
	voiceMsg := &tele.Voice{
		File: tele.FromReader(bytes.NewReader(oggBytes)),
		MIME: "audio/ogg",
	}

	if _, err := c.Bot().Send(c.Recipient(), voiceMsg); err != nil {
		log.Error("sending voice reply", "err", err)
	}
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
		log.Info("resumed conversation", "id", existing)
		return existing
	}

	// No existing conversation. Create a new one.
	newID := fmt.Sprintf("tg_%d_%d", chatID, time.Now().Unix())
	b.conversationIDs.Store(key, newID)
	return newID
}

// handleHelp shows all available commands.
func (b *Bot) handleHelp(c tele.Context) error {
	msg := "\U0001F4D6 <b>Commands</b>\n\n" +
		"<b>Conversation</b>\n" +
		"/clear — start a fresh conversation\n" +
		"/compact — summarize older messages to free up context\n\n" +
		"<b>Memory</b>\n" +
		"/facts — list all remembered facts\n" +
		"/forget <code>&lt;id&gt;</code> — forget a specific fact\n\n" +
		"<b>Persona</b>\n" +
		"/persona — view Mira's current personality\n" +
		"/reflect — trigger a reflection on recent conversations\n\n" +
		"<b>Reminders</b>\n" +
		"/remind <code>&lt;time&gt; &lt;message&gt;</code> — set a reminder\n" +
		"/schedule — list upcoming reminders\n\n" +
		"<b>Info</b>\n" +
		"/stats — token usage, cost, and message counts\n" +
		"/status — uptime, models, and service health\n\n" +
		"<b>System</b>\n" +
		"/traces — toggle agent thinking traces in chat\n" +
		"/restart — restart the bot process\n" +
		"/help — this message\n\n" +
		"<b>Features</b>\n" +
		"Send a photo and Mira will describe what she sees.\n" +
		"Just chat normally — she remembers your conversations."
	return c.Send(msg, &tele.SendOptions{ParseMode: tele.ModeHTML})
}

// handleClear resets the conversation context.
func (b *Bot) handleClear(c tele.Context) error {
	chatID := c.Message().Chat.ID
	key := fmt.Sprintf("%d", chatID)

	newID := fmt.Sprintf("tg_%d_%d", chatID, time.Now().Unix())
	b.conversationIDs.Store(key, newID)

	log.Info("/clear: conversation reset", "chat", chatID, "new_id", newID)
	return c.Send("Context cleared. Fresh start!")
}

// makeTraceCallback creates a closure that sends/edits the agent trace
// message in Telegram. First call sends a new message; subsequent calls
// edit it with the accumulated trace text. The message uses HTML parse
// mode for formatting (bold tool names, italic thinking, etc.).
//
// This is the same closure pattern as statusCallback and sendCallback —
// the returned function "closes over" the traceMsg variable so it
// always knows which message to edit.
func (b *Bot) makeTraceCallback(c tele.Context) agent.TraceCallback {
	// Pre-send a placeholder so the trace message is ABOVE the reply
	// in chat order. It gets replaced on the first real trace update.
	// Uses a short-timeout client so a Telegram blip doesn't stall the
	// entire agent pipeline (the 60s default caused an 85s stall).
	traceMsg, err := c.Bot().Send(c.Recipient(), "🧠")
	if err != nil {
		log.Warn("trace: failed to send placeholder", "err", err)
		traceMsg = nil
	}

	// All trace operations run in a goroutine so they NEVER block the
	// agent loop. Traces are observability — not critical path. A mutex
	// preserves ordering so rapid tool calls don't race.
	var mu sync.Mutex
	return func(text string) error {
		go func() {
			mu.Lock()
			defer mu.Unlock()

			if traceMsg == nil {
				msg, err := c.Bot().Send(c.Recipient(), text, &tele.SendOptions{ParseMode: tele.ModeHTML})
				if err != nil {
					log.Warn("trace: send failed", "err", err)
					msg, err = c.Bot().Send(c.Recipient(), stripHTML(text))
					if err != nil {
						log.Warn("trace: plain send also failed", "err", err)
						return
					}
				}
				traceMsg = msg
			} else {
				_, err := c.Bot().Edit(traceMsg, text, &tele.SendOptions{ParseMode: tele.ModeHTML})
				if err != nil {
					if strings.Contains(err.Error(), "not modified") {
						return
					}
					log.Warn("trace: edit failed, retrying plain", "err", err)
					_, err = c.Bot().Edit(traceMsg, stripHTML(text))
					if err != nil && !strings.Contains(err.Error(), "not modified") {
						log.Warn("trace: plain edit also failed", "err", err)
					}
				}
			}
		}()
		return nil
	}
}

// handleTraces toggles agent thinking traces on/off.
// When enabled, Mira sends a separate message before each reply showing
// the agent's tool calls, thinking, and decision-making process.
func (b *Bot) handleTraces(c tele.Context) error {
	newState := !b.cfg.Agent.Trace
	if err := b.cfg.SetTrace(b.configPath, newState); err != nil {
		log.Error("/traces: failed to update config", "err", err)
		return c.Send(fmt.Sprintf("Failed to update config: %v", err))
	}
	if newState {
		return c.Send("🧠 Agent traces <b>enabled</b> — you'll see thinking traces before each reply.", &tele.SendOptions{ParseMode: tele.ModeHTML})
	}
	return c.Send("🧠 Agent traces <b>disabled</b>.", &tele.SendOptions{ParseMode: tele.ModeHTML})
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

	log.Info("/forget: deactivated fact", "fact_id", factID)
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
		log.Error("manual reflection", "err", err)
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
	visionStatus := "off"
	if b.visionLLM != nil {
		visionStatus = "on"
	}

	// Check voice sidecars via health endpoints.
	sttStatus := "off"
	if b.voiceClient != nil {
		if b.voiceClient.IsAvailable() {
			sttStatus = "running"
		} else {
			sttStatus = "not responding"
		}
	}
	ttsStatus := "off"
	if b.ttsClient != nil {
		if b.ttsClient.IsAvailable() {
			ttsStatus = "running"
		} else {
			ttsStatus = "not responding"
		}
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
			"  Agent: %s\n"+
			"  Vision: %s\n\n"+
			"<b>Services:</b>\n"+
			"  Embeddings: %s\n"+
			"  Web search: %s\n"+
			"  Vision: %s\n\n"+
			"<b>Voice:</b>\n"+
			"  STT (Parakeet): %s [%s]\n"+
			"  TTS (Piper): %s [%s]\n\n"+
			"<b>Session:</b>\n"+
			"  Messages: %d\n"+
			"  Facts: %d\n"+
			"  Cost: $%.4f\n\n"+
			"<b>Chat ID:</b> <code>%d</code>",
		uptime, managedBy, runtime.Version(), convID,
		b.cfg.LLM.Model, b.cfg.Agent.Model, b.cfg.Vision.Model,
		embedStatus, tavilyStatus, visionStatus,
		sttStatus, b.cfg.Voice.STT.Model,
		ttsStatus, b.cfg.Voice.TTS.VoiceID,
		stats.TotalMessages, stats.TotalFacts, stats.TotalCostUSD,
		c.Message().Chat.ID,
	)
	return c.Send(msg, &tele.SendOptions{ParseMode: tele.ModeHTML})
}

// handleRestart restarts the bot process. If running under launchd,
// uses launchctl to do a clean restart. Otherwise, exits and relies
// on the user to restart manually.
func (b *Bot) handleRestart(c tele.Context) error {
	log.Info("/restart: restart requested via Telegram")

	if isLaunchdManaged() {
		_ = c.Send("Restarting via launchd... be right back.")

		// launchctl kickstart -k forces a restart of the service.
		// The -k flag kills the existing instance first.
		go func() {
			time.Sleep(500 * time.Millisecond) // let the message send
			cmd := exec.Command("launchctl", "kickstart", "-k", "gui/"+fmt.Sprintf("%d", os.Getuid())+"/com.mira.her-go")
			if err := cmd.Run(); err != nil {
				log.Error("launchctl kickstart failed, falling back to exit", "err", err)
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

// handleRemind routes reminder requests through the agent pipeline.
// Instead of trying to parse natural language time ourselves (which is
// brittle), we let the LLM do what it's good at — understanding
// "in 2 mins", "tomorrow at 3pm", "next friday morning", etc.
//
// The agent sees the text as a normal message, recognizes the reminder
// intent, and calls the create_reminder tool with a proper ISO timestamp.
// This means /remind is really just a convenience shortcut — the user
// could also just say "remind me to call the dentist at 3pm" in normal
// conversation and the agent would do the same thing.
func (b *Bot) handleRemind(c tele.Context) error {
	args := strings.TrimSpace(c.Message().Payload)
	if args == "" {
		return c.Send(
			"<b>Usage:</b> /remind <code>&lt;time&gt; &lt;message&gt;</code>\n\n"+
				"<b>Examples:</b>\n"+
				"/remind 3pm call the dentist\n"+
				"/remind tomorrow at 10am take out the trash\n"+
				"/remind in 30 minutes check the oven\n"+
				"/remind next friday review the report",
			&tele.SendOptions{ParseMode: tele.ModeHTML},
		)
	}

	// Rewrite the command as a natural message and feed it through
	// the agent pipeline. The agent will parse the time, call
	// create_reminder, and reply with a confirmation.
	c.Message().Text = "remind me " + args
	return b.handleMessage(c)
}

// handleSchedule lists active scheduled tasks or manages them.
// Usage:
//
//	/schedule          — list all active tasks
//	/schedule pause N  — disable task #N
//	/schedule resume N — re-enable task #N
//	/schedule delete N — remove task #N
func (b *Bot) handleSchedule(c tele.Context) error {
	args := strings.TrimSpace(c.Message().Payload)

	// Sub-commands: pause, resume, delete.
	if args != "" {
		parts := strings.Fields(args)
		if len(parts) >= 2 {
			action := strings.ToLower(parts[0])
			taskID, err := strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				return c.Send("Usage: /schedule <pause|resume|delete> <id>")
			}

			switch action {
			case "pause":
				if err := b.store.UpdateScheduledTaskEnabled(taskID, false); err != nil {
					return c.Send(fmt.Sprintf("Couldn't pause task #%d: %v", taskID, err))
				}
				return c.Send(fmt.Sprintf("⏸ Paused task #%d.", taskID))

			case "resume":
				if err := b.store.UpdateScheduledTaskEnabled(taskID, true); err != nil {
					return c.Send(fmt.Sprintf("Couldn't resume task #%d: %v", taskID, err))
				}
				return c.Send(fmt.Sprintf("▶️ Resumed task #%d.", taskID))

			case "delete":
				if err := b.store.DeleteScheduledTask(taskID); err != nil {
					return c.Send(fmt.Sprintf("Couldn't delete task #%d: %v", taskID, err))
				}
				return c.Send(fmt.Sprintf("🗑 Deleted task #%d.", taskID))

			default:
				return c.Send("Unknown action. Try: /schedule pause|resume|delete <id>")
			}
		}
	}

	// Default: list all active tasks.
	tasks, err := b.store.ListActiveTasks()
	if err != nil {
		log.Error("/schedule: listing tasks", "err", err)
		return c.Send("Couldn't load scheduled tasks right now.")
	}

	if len(tasks) == 0 {
		return c.Send("No scheduled tasks. Use /remind to create one!")
	}

	// Load timezone for display.
	loc, err := time.LoadLocation(b.cfg.Scheduler.Timezone)
	if err != nil {
		loc = time.UTC
	}

	var sb strings.Builder
	sb.WriteString("<b>📋 Scheduled Tasks</b>\n\n")

	for _, t := range tasks {
		name := "unnamed"
		if t.Name != nil {
			name = *t.Name
		}

		nextRun := "—"
		if t.NextRun != nil {
			nextRun = t.NextRun.In(loc).Format("Mon Jan 2 at 3:04 PM")
		}

		sb.WriteString(fmt.Sprintf(
			"<b>#%d</b> %s\n  ⏰ %s | type: %s\n\n",
			t.ID, name, nextRun, t.TaskType,
		))
	}

	sb.WriteString("<i>/schedule pause|resume|delete &lt;id&gt;</i>")

	return c.Send(sb.String(), &tele.SendOptions{ParseMode: tele.ModeHTML})
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
	if memCtx, err := memory.BuildMemoryContext(b.store, b.cfg.Memory.MaxFactsInContext, nil); err == nil && memCtx != "" {
		parts = append(parts, memCtx)
	}

	return strings.Join(parts, "\n\n---\n\n")
}

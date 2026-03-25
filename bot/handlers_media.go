// Package bot — media message handlers (photo, voice).
package bot

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"her/agent"
	"her/scrub"

	tele "gopkg.in/telebot.v4"
)

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

	stageResetCallback := func() error {
		newPlaceholder, err := c.Bot().Send(c.Recipient(), "\U0001F4AD")
		if err != nil {
			return fmt.Errorf("stage reset: sending new placeholder: %w", err)
		}
		placeholder = newPlaceholder
		return nil
	}
	deletePlaceholderCallback := func() error {
		return c.Bot().Delete(placeholder)
	}

	// Run the agent with image data attached.
	result, err := agent.Run(agent.RunParams{
		AgentLLM:                  b.agentLLM,
		ChatLLM:                   b.llm,
		VisionLLM:                 b.visionLLM,
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
		TraceCallback:             traceCallback,
		ReflectionThreshold:       b.cfg.Persona.ReflectionMemoryThreshold,
		RewriteEveryN:             b.cfg.Persona.RewriteEveryNReflections,
		ImageBase64:               imageBase64,
		ImageMIME:                 imageMIME,
	})

	close(stopTyping)

	if err != nil {
		log.Error("agent error", "err", err)
		_, _ = c.Bot().Edit(placeholder, "Sorry, I'm having trouble thinking right now. Try again in a moment?")
		return nil
	}

	log.Infof("  %s: %s", strings.ToLower(b.cfg.Identity.Her), truncate(result.ReplyText, 100))
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
	stageResetCallback := func() error {
		newPlaceholder, err := c.Bot().Send(c.Recipient(), "\U0001F4AD")
		if err != nil {
			return fmt.Errorf("stage reset: sending new placeholder: %w", err)
		}
		placeholder = newPlaceholder
		return nil
	}
	deletePlaceholderCallback := func() error {
		return c.Bot().Delete(placeholder)
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
		AgentLLM:                  b.agentLLM,
		ChatLLM:                   b.llm,
		VisionLLM:                 b.visionLLM,
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
		TTSCallback:               ttsCallback,
		TraceCallback:             traceCallback,
		ReflectionThreshold:       b.cfg.Persona.ReflectionMemoryThreshold,
		RewriteEveryN:             b.cfg.Persona.RewriteEveryNReflections,
	})

	close(stopTyping)

	if err != nil {
		log.Error("agent error", "err", err)
		_, _ = c.Bot().Edit(placeholder, "Sorry, I'm having trouble thinking right now. Try again in a moment?")
		return nil
	}

	log.Infof("  %s: %s", strings.ToLower(b.cfg.Identity.Her), truncate(result.ReplyText, 100))

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

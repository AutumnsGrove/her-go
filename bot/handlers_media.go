// Package bot — media message handlers (photo, voice).
package bot

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"her/ocr"

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

	// Step 4: Pre-flight OCR — extract text from the image before the
	// agent runs. This is free (Apple Vision Neural Engine, sub-200ms)
	// and lets the agent triage photos without a VLM call. Receipts get
	// routed to scan_receipt, other images fall through to view_image.
	var ocrText string
	if b.ocrEnabled {
		ocrResult, ocrErr := ocr.Extract(imageBytes, &b.cfg.OCR)
		if ocrErr != nil {
			log.Warn("pre-flight OCR failed (non-fatal)", "err", ocrErr)
		} else if ocrResult != nil && ocrResult.Text != "" {
			ocrText = ocrResult.Text
			log.Infof("  OCR: %d chars, %.2f confidence (%s)", len(ocrText), ocrResult.Confidence, ocrResult.Engine)
		} else {
			log.Info("  OCR: no text detected")
		}
	}

	// Step 5: Build the user message text.
	// The agent sees this as the "user said" content. The image itself
	// travels separately via RunParams.ImageBase64.
	userText := "[User sent a photo]"
	if caption != "" {
		userText = "[User sent a photo] " + caption
	}

	// From here, same pipeline as handleMessage: save, scrub, run agent.
	// CRITICAL: If SaveMessage fails, we must NOT call runAgent with msgID=0.
	// That creates orphaned TUI turns that attach to the wrong section.
	msgID, err := b.store.SaveMessage("user", userText, "", conversationID)
	if err != nil {
		log.Error("saving message", "err", err)
		return c.Send("I couldn't save that photo. Try sending it again?")
	}
	if msgID == 0 {
		log.Error("SaveMessage returned msgID=0 without error")
		return c.Send("Something went wrong saving that photo. Try again?")
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
	scrubResult := b.scrubText(userText)

	if msgID > 0 {
		b.store.UpdateMessageScrubbed(msgID, scrubResult.Text)
		b.savePIIVaultEntries(msgID, scrubResult.Vault)
	}

	// Run the agent with image data attached.
	return b.runAgent(c, AgentInput{
		UserMessage:    userText,
		ScrubbedText:   scrubResult.Text,
		ScrubVault:     scrubResult.Vault,
		ConversationID: conversationID,
		TriggerMsgID:   msgID,
		ImageBase64:    imageBase64,
		ImageMIME:      imageMIME,
		OCRText:        ocrText,
	})
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

	// Step 5: Save the transcribed text as the user message. From here,
	// the transcript IS the user message — same as if they typed it,
	// except we also store the voice memo path.
	// CRITICAL: If SaveMessage fails, we must NOT call runAgent with msgID=0.
	// Also must close stopTyping to avoid goroutine leak.
	userText := transcript

	msgID, err := b.store.SaveMessage("user", userText, "", conversationID)
	if err != nil {
		close(stopTyping)
		log.Error("saving message", "err", err)
		return c.Send("I couldn't save that voice memo. Try sending it again?")
	}
	if msgID == 0 {
		close(stopTyping)
		log.Error("SaveMessage returned msgID=0 without error")
		return c.Send("Something went wrong saving that voice memo. Try again?")
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
	scrubResult := b.scrubText(userText)

	if msgID > 0 {
		b.store.UpdateMessageScrubbed(msgID, scrubResult.Text)
		b.savePIIVaultEntries(msgID, scrubResult.Vault)
	}

	// Run the agent pipeline. The voice handler customizes two things:
	//  - PlaceholderText: shows the transcript while thinking
	//  - ForceTTS: always reply with voice (they sent a voice memo)
	return b.runAgent(c, AgentInput{
		UserMessage:     "\U0001F3A4 " + userText,
		ScrubbedText:    scrubResult.Text,
		ScrubVault:      scrubResult.Vault,
		ConversationID:  conversationID,
		TriggerMsgID:    msgID,
		PlaceholderText: fmt.Sprintf("\U0001F3A4 <i>%s</i>\n\n\U0001F4AD", transcript),
		PlaceholderHTML: true,
		ForceTTS:        true,
	})
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

// Package bot — admin and system management handlers.
package bot

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"her/compact"

	tele "gopkg.in/telebot.v4"
)

// handleCompact manually triggers conversation compaction.
func (b *Bot) handleCompact(c tele.Context) error {
	convID := b.getConversationID(c.Message().Chat.ID)
	recent, err := b.store.RecentMessages(convID, b.cfg.Memory.RecentMessages)
	if err != nil || len(recent) < 4 {
		return c.Send("Not enough messages to compact yet.")
	}

	tokensBefore := compact.EstimateHistoryTokens("", recent)

	// Force compaction by passing a very low threshold (0 = always compact).
	cr, err := compact.MaybeCompact(b.llm, b.store, convID, recent, 1, b.cfg.Identity.Her, b.cfg.Identity.User)
	if err != nil {
		return c.Send(fmt.Sprintf("Compaction failed: %v", err))
	}

	tokensAfter := compact.EstimateHistoryTokens(cr.Summary, cr.KeptMessages)
	saved := tokensBefore - tokensAfter

	msg := fmt.Sprintf(
		"\U0001F5DC <b>Compacted</b>\n\n"+
			"Messages: %d \u2192 %d kept\n"+
			"Tokens: ~%d \u2192 ~%d (saved ~%d)\n\n"+
			"<i>Summary:</i>\n%s",
		len(recent), len(cr.KeptMessages),
		tokensBefore, tokensAfter, saved,
		cr.Summary,
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
	if os.Getenv("__CFBundleIdentifier") != "" || isLaunchdManaged(b.cfg.Identity.Her) {
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
		b.cfg.Chat.Model, b.cfg.Agent.Model, b.cfg.Vision.Model,
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

	if isLaunchdManaged(b.cfg.Identity.Her) {
		_ = c.Send("Restarting via launchd... be right back.")

		// launchctl kickstart -k forces a restart of the service.
		// The -k flag kills the existing instance first.
		go func() {
			time.Sleep(500 * time.Millisecond) // let the message send
			cmd := exec.Command("launchctl", "kickstart", "-k", "gui/"+fmt.Sprintf("%d", os.Getuid())+"/com."+strings.ToLower(b.cfg.Identity.Her)+".her-go")
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
func isLaunchdManaged(botName string) bool {
	cmd := exec.Command("launchctl", "print", "gui/"+fmt.Sprintf("%d", os.Getuid())+"/com."+strings.ToLower(botName)+".her-go")
	return cmd.Run() == nil
}

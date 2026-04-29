// Package bot — admin and system management handlers.
package bot

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
		b.cfg.Chat.Model, b.cfg.Driver.Model, b.cfg.Vision.Model,
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

// handleUpdate pulls the latest code, builds a new binary, and restarts.
// This is the self-update mechanism for the Mac Mini production instance.
// The flow: git pull → go build → backup old binary → swap → restart via
// launchd. Build failures are caught before the swap — the bot keeps
// running with the old binary if compilation fails.
func (b *Bot) handleUpdate(c tele.Context) error {
	log.Info("/update: self-update requested via Telegram")

	if !isLaunchdManaged(b.cfg.Identity.Her) {
		return c.Send("⚠️ /update only works when running as a launchd service. Use <code>her setup</code> first.", &tele.SendOptions{ParseMode: tele.ModeHTML})
	}

	// Determine repo and binary paths.
	repoPath := b.cfg.Update.RepoPath
	if repoPath == "" {
		var err error
		repoPath, err = os.Getwd()
		if err != nil {
			return c.Send(fmt.Sprintf("❌ Failed to get working directory: %v", err))
		}
	}
	binaryPath := filepath.Join(repoPath, "her-go")

	// Step 1: git pull
	_ = c.Send("📥 Pulling changes...")
	pullOut, err := exec.Command("git", "-C", repoPath, "pull", "origin", "main").CombinedOutput()
	pullMsg := strings.TrimSpace(string(pullOut))

	if err != nil {
		return c.Send(fmt.Sprintf("❌ Pull failed:\n<pre>%s</pre>", truncateForTelegram(pullMsg, 3800)), &tele.SendOptions{ParseMode: tele.ModeHTML})
	}

	if strings.Contains(pullMsg, "Already up to date") {
		return c.Send("✅ Already up to date.")
	}

	// Step 2: go build
	_ = c.Send(fmt.Sprintf("🔨 Building...\n<pre>%s</pre>", truncateForTelegram(pullMsg, 1000)), &tele.SendOptions{ParseMode: tele.ModeHTML})
	nextBinary := binaryPath + ".next"
	buildCmd := exec.Command("go", "build", "-o", nextBinary, ".")
	buildCmd.Dir = repoPath
	buildOut, err := buildCmd.CombinedOutput()
	if err != nil {
		// Build failed — bot keeps running with old binary.
		_ = os.Remove(nextBinary)
		buildMsg := strings.TrimSpace(string(buildOut))
		return c.Send(fmt.Sprintf("❌ Build failed:\n<pre>%s</pre>", truncateForTelegram(buildMsg, 3800)), &tele.SendOptions{ParseMode: tele.ModeHTML})
	}

	// Step 3: Backup old binary.
	backupPath := binaryPath + ".backup"
	if err := copyFile(binaryPath, backupPath); err != nil {
		_ = os.Remove(nextBinary)
		return c.Send(fmt.Sprintf("❌ Backup failed: %v", err))
	}

	// Step 4: Atomic swap — rename new binary over old.
	if err := os.Rename(nextBinary, binaryPath); err != nil {
		_ = os.Remove(nextBinary)
		return c.Send(fmt.Sprintf("❌ Swap failed: %v", err))
	}

	// Step 5: Write the pending flag so the new binary can confirm on startup.
	flagPath := filepath.Join(repoPath, "her.update_pending")
	flagContent := fmt.Sprintf("✅ Updated and restarted successfully.\n\n<pre>%s</pre>", truncateForTelegram(pullMsg, 2000))
	_ = os.WriteFile(flagPath, []byte(flagContent), 0644)

	// Step 6: Restart via launchd.
	_ = c.Send("🔄 Restarting...")

	go func() {
		time.Sleep(500 * time.Millisecond) // let the message send
		label := "gui/" + fmt.Sprintf("%d", os.Getuid()) + "/com." + strings.ToLower(b.cfg.Identity.Her) + ".her-go"
		cmd := exec.Command("launchctl", "kickstart", "-k", label)
		if err := cmd.Run(); err != nil {
			log.Error("launchctl kickstart failed, falling back to exit", "err", err)
			os.Exit(0) // launchd will restart us via KeepAlive
		}
	}()

	return nil
}

// CheckUpdatePending checks for a her.update_pending flag file on startup
// and sends the stored confirmation message to the owner chat. This bridges
// across process lifetimes — the old binary writes the flag before dying,
// the new binary reads it after starting.
func (b *Bot) CheckUpdatePending() {
	repoPath := b.cfg.Update.RepoPath
	if repoPath == "" {
		repoPath, _ = os.Getwd()
	}
	flagPath := filepath.Join(repoPath, "her.update_pending")

	data, err := os.ReadFile(flagPath)
	if err != nil {
		return // no pending update — normal startup
	}

	// Remove the flag before sending — even if the send fails, we don't
	// want to re-send on every restart.
	_ = os.Remove(flagPath)

	msg := strings.TrimSpace(string(data))
	if msg == "" {
		return
	}

	if b.ownerChat == 0 {
		log.Warn("update pending but no owner_chat configured — can't send confirmation")
		return
	}

	if err := b.SendToChat(b.ownerChat, msg); err != nil {
		log.Error("failed to send update confirmation", "err", err)
	}
}

// truncateForTelegram truncates a string to fit within a Telegram message,
// adding an ellipsis if truncated. maxLen should leave room for surrounding
// formatting (message header, HTML tags, etc.).
func truncateForTelegram(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "\n... (truncated)"
}

// copyFile copies src to dst. Used for creating the backup binary before swap.
func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	// Preserve the executable permission bits from the source.
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, info.Mode())
}

// isLaunchdManaged checks if the bot is running as a launchd service
// by looking for the service in launchctl.
func isLaunchdManaged(botName string) bool {
	cmd := exec.Command("launchctl", "print", "gui/"+fmt.Sprintf("%d", os.Getuid())+"/com."+strings.ToLower(botName)+".her-go")
	return cmd.Run() == nil
}

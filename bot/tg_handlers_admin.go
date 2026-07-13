// Package bot — Telegram-specific admin handlers.
//
// These commands stay registered with telebot because they use
// Telegram-specific features (multi-step progress messages, process
// management). Transport-neutral commands are in commands_gateway.go.
package bot

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"her/procmgr"

	tele "gopkg.in/telebot.v4"
)

// restartFlag is persisted to disk before a restart so the new process
// can edit the "Restarting..." message to confirm it's back online.
type restartFlag struct {
	ChatID    int64  `json:"chat_id"`
	MessageID int    `json:"message_id"`
	Extra     string `json:"extra,omitempty"` // e.g. git pull output for /update
}

const restartFlagFile = "her.restart_pending"

// handleRestart restarts the bot process. If running under a process
// supervisor (launchd or systemd), uses the supervisor to restart.
// Otherwise, exits and relies on the user to restart manually.
func (b *Bot) handleRestart(c tele.Context) error {
	log.Info("/restart: restart requested via Telegram")

	if mgr := b.processManager(); mgr != nil && mgr.IsManaged() {
		msg, _ := b.tb.Send(c.Chat(), fmt.Sprintf("🔄 Restarting via %s…", mgr.Name()))
		b.writeRestartFlag(c.Chat().ID, msg, "")

		go func() {
			time.Sleep(500 * time.Millisecond)
			if err := mgr.Restart(); err != nil {
				log.Error("supervisor restart failed, falling back to exit", "err", err)
				os.Exit(0)
			}
		}()
		return nil
	}

	_ = c.Send("Shutting down. Restart me manually with `go run main.go`.")
	go func() {
		time.Sleep(500 * time.Millisecond)
		b.Stop()
	}()
	return nil
}

// handleUpdate pulls the latest code, builds a new binary, and restarts.
// This is the self-update mechanism for production instances.
// The flow: git pull → go build → backup old binary → swap → restart via
// supervisor. Build failures are caught before the swap — the bot keeps
// running with the old binary if compilation fails.
func (b *Bot) handleUpdate(c tele.Context) error {
	log.Info("/update: self-update requested via Telegram")

	mgr := b.processManager()
	if mgr == nil || !mgr.IsManaged() {
		return c.Send("⚠️ /update only works when running as a managed service. Use <code>her setup</code> first.", &tele.SendOptions{ParseMode: tele.ModeHTML})
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

	// Auto-detect binary name by checking which file exists.
	// Supports both "her" (standard) and "her-go" (legacy).
	var binaryPath string
	if _, err := os.Stat(filepath.Join(repoPath, "her")); err == nil {
		binaryPath = filepath.Join(repoPath, "her")
	} else if _, err := os.Stat(filepath.Join(repoPath, "her-go")); err == nil {
		binaryPath = filepath.Join(repoPath, "her-go")
	} else {
		return c.Send("❌ Binary not found (checked 'her' and 'her-go')")
	}

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

	// Step 5: Send "Restarting..." and save the flag so the new binary
	// can edit this message to confirm it's back online.
	msg, _ := b.tb.Send(c.Chat(), "🔄 Restarting…")
	extra := fmt.Sprintf("<pre>%s</pre>", truncateForTelegram(pullMsg, 2000))
	b.writeRestartFlag(c.Chat().ID, msg, extra)

	// Step 6: Restart via supervisor.
	go func() {
		time.Sleep(500 * time.Millisecond)
		if err := mgr.Restart(); err != nil {
			log.Error("supervisor restart failed, falling back to exit", "err", err)
			os.Exit(0)
		}
	}()

	return nil
}

// writeRestartFlag persists the chat/message IDs so the new process can
// edit the "Restarting..." message to confirm it's back. msg may be nil
// if the send failed (graceful degradation — restart still happens).
func (b *Bot) writeRestartFlag(chatID int64, msg *tele.Message, extra string) {
	if msg == nil {
		return
	}
	repoPath := b.cfg.Update.RepoPath
	if repoPath == "" {
		repoPath, _ = os.Getwd()
	}
	flag := restartFlag{ChatID: chatID, MessageID: msg.ID, Extra: extra}
	data, _ := json.Marshal(flag)
	_ = os.WriteFile(filepath.Join(repoPath, restartFlagFile), data, 0644)
}

// CheckRestartPending looks for a restart flag file on startup and edits
// the original "Restarting..." message to confirm the bot is back online.
// This bridges across process lifetimes — the old process writes the flag
// before dying, the new process reads it after booting.
func (b *Bot) CheckRestartPending() {
	repoPath := b.cfg.Update.RepoPath
	if repoPath == "" {
		repoPath, _ = os.Getwd()
	}
	flagPath := filepath.Join(repoPath, restartFlagFile)

	data, err := os.ReadFile(flagPath)
	if err != nil {
		return
	}
	_ = os.Remove(flagPath)

	// Also clean up the old-style update flag if present.
	_ = os.Remove(filepath.Join(repoPath, "her.update_pending"))

	var flag restartFlag
	if err := json.Unmarshal(data, &flag); err != nil || flag.ChatID == 0 || flag.MessageID == 0 {
		return
	}

	confirmation := "✅ Back online."
	if flag.Extra != "" {
		confirmation = "✅ Updated and back online.\n\n" + flag.Extra
	}

	target := &tele.Message{
		ID:   flag.MessageID,
		Chat: &tele.Chat{ID: flag.ChatID},
	}
	_, err = b.tb.Edit(target, confirmation, &tele.SendOptions{ParseMode: tele.ModeHTML})
	if err != nil {
		log.Warn("failed to edit restart confirmation", "err", err)
		_ = b.SendToChat(flag.ChatID, confirmation)
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

// processManager returns a procmgr.Manager for the current platform.
// Returns nil if the platform is unsupported (shouldn't happen on
// macOS or Linux).
func (b *Bot) processManager() procmgr.Manager {
	label := procmgr.EffectiveLabel(b.cfg.Update.ServiceLabel, b.cfg.Identity.Her)
	mgr, err := procmgr.New(label)
	if err != nil {
		return nil
	}
	return mgr
}

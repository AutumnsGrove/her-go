// Package bot — Telegram-specific admin handlers.
//
// These commands stay registered with telebot because they use
// Telegram-specific features (multi-step progress messages, process
// management). Transport-neutral commands are in commands_gateway.go.
package bot

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"her/procmgr"

	tele "gopkg.in/telebot.v4"
)

// handleRestart restarts the bot process. If running under a process
// supervisor (launchd or systemd), uses the supervisor to restart.
// Otherwise, exits and relies on the user to restart manually.
func (b *Bot) handleRestart(c tele.Context) error {
	log.Info("/restart: restart requested via Telegram")

	if mgr := b.processManager(); mgr != nil && mgr.IsManaged() {
		_ = c.Send(fmt.Sprintf("Restarting via %s... be right back.", mgr.Name()))

		go func() {
			time.Sleep(500 * time.Millisecond) // let the message send
			if err := mgr.Restart(); err != nil {
				log.Error("supervisor restart failed, falling back to exit", "err", err)
				os.Exit(0) // supervisor's Restart=always / KeepAlive will bring us back
			}
		}()
		return nil
	}

	// Not managed by a supervisor. Just exit cleanly.
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

	// Step 6: Restart via supervisor.
	_ = c.Send("🔄 Restarting...")

	go func() {
		time.Sleep(500 * time.Millisecond) // let the message send
		if err := mgr.Restart(); err != nil {
			log.Error("supervisor restart failed, falling back to exit", "err", err)
			os.Exit(0) // supervisor will bring us back (KeepAlive / Restart=always)
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

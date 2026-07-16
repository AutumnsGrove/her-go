package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"her/logger"
	"her/procmgr"

	"github.com/spf13/cobra"
)

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Pull latest code, rebuild, and restart the service",
	Long: `Pulls the latest code from the main branch, rebuilds the binary,
and restarts the service. This is the CLI equivalent of the /update
Telegram command, useful for remote SSH management.

Steps:
  1. git pull origin main
  2. go build -o her
  3. Restart service (systemd/launchd)`,
	RunE: runUpdate,
}

func init() {
	rootCmd.AddCommand(updateCmd)
}

func runUpdate(cmd *cobra.Command, args []string) error {
	log := logger.WithPrefix("update")

	// Get current working directory (repo root)
	repoPath, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get working directory: %w", err)
	}

	binaryPath := filepath.Join(repoPath, "her")

	// Step 1: git pull
	log.Info("📥 Pulling changes from origin/main...")
	pullCmd := exec.Command("git", "pull", "origin", "main")
	pullCmd.Dir = repoPath
	pullOut, err := pullCmd.CombinedOutput()
	if err != nil {
		fmt.Printf("❌ Pull failed:\n%s\n", string(pullOut))
		return fmt.Errorf("git pull failed: %w", err)
	}
	fmt.Printf("📥 %s\n", string(pullOut))

	// Step 2: go build
	log.Info("🔨 Building...")
	buildCmd := exec.Command("go", "build", "-ldflags=-s -w", "-o", "her", ".")
	buildCmd.Dir = repoPath
	buildOut, err := buildCmd.CombinedOutput()
	if err != nil {
		fmt.Printf("❌ Build failed:\n%s\n", string(buildOut))
		return fmt.Errorf("go build failed: %w", err)
	}
	fmt.Println("✅ Build successful")

	// Step 3: Restart service
	log.Info("🔄 Restarting service...")
	// Use "her-go" as default label (same as setup command)
	mgr, err := procmgr.New("her-go")
	if err != nil {
		return fmt.Errorf("failed to create process manager: %w", err)
	}

	if !mgr.IsManaged() {
		fmt.Println("⚠️  Service is not managed by systemd/launchd. Restart manually.")
		fmt.Printf("✅ Binary updated at: %s\n", binaryPath)
		return nil
	}

	if err := mgr.Restart(); err != nil {
		return fmt.Errorf("failed to restart service: %w", err)
	}

	fmt.Printf("✅ Service restarted successfully\n")
	log.Info("update complete", "binary", binaryPath)

	return nil
}

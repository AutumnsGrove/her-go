package cmd

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"her/config"

	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show launchd service status",
	RunE:  runStatus,
}

func init() {
	rootCmd.AddCommand(statusCmd)
}

func runStatus(cmd *cobra.Command, args []string) error {
	botName, err := loadBotName()
	if err != nil {
		return err
	}
	label := serviceLabel(botName)

	// Use the serviceTarget helper to build the launchctl print target.
	target := serviceTarget(label)

	out, err := exec.Command("launchctl", "print", target).CombinedOutput()
	if err != nil {
		fmt.Println("Service is not loaded.")
		fmt.Println()
		fmt.Println("Run 'her setup' to install, or 'her start' to load.")
		return nil
	}

	output := string(out)

	// Parse key fields from launchctl print output.
	fmt.Printf("Service: %s\n", label)
	fmt.Println()

	// State (e.g., "state = running")
	if val := parseLaunchctlField(output, "state"); val != "" {
		fmt.Printf("  State:       %s\n", val)
	}

	// PID
	if val := parseLaunchctlField(output, "pid"); val != "" {
		fmt.Printf("  PID:         %s\n", val)
	}

	// Last exit status
	if val := parseLaunchctlField(output, "last exit code"); val != "" {
		fmt.Printf("  Last exit:   %s\n", val)
	}

	// Log paths — derive from working directory.
	workDir, _ := os.Getwd()
	fmt.Printf("  Stdout log:  %s/logs/stdout.log\n", workDir)
	fmt.Printf("  Stderr log:  %s/logs/stderr.log\n", workDir)

	// Also show run count if available.
	if val := parseLaunchctlField(output, "runs"); val != "" {
		fmt.Printf("  Run count:   %s\n", val)
	}

	// Show sidecar status by hitting their health endpoints.
	cfg, err := config.Load("config.yaml")
	if err == nil {
		fmt.Println()
		fmt.Println("Sidecars:")
		httpClient := &http.Client{Timeout: 2 * time.Second}

		// Parakeet STT
		sttStatus := "disabled"
		if cfg.Voice.Enabled && cfg.Voice.STT.BaseURL != "" {
			sttURL := strings.TrimRight(cfg.Voice.STT.BaseURL, "/") + "/healthz"
			if resp, err := httpClient.Get(sttURL); err == nil {
				resp.Body.Close()
				sttStatus = fmt.Sprintf("running (%s)", cfg.Voice.STT.BaseURL)
			} else {
				sttStatus = "not responding"
			}
		}
		fmt.Printf("  Parakeet STT:  %s  [model: %s]\n", sttStatus, cfg.Voice.STT.Model)

		// Piper TTS
		ttsStatus := "disabled"
		if cfg.Voice.TTS.Enabled && cfg.Voice.TTS.BaseURL != "" {
			ttsURL := strings.TrimRight(cfg.Voice.TTS.BaseURL, "/") + "/healthz"
			if resp, err := httpClient.Get(ttsURL); err == nil {
				resp.Body.Close()
				ttsStatus = fmt.Sprintf("running (%s)", cfg.Voice.TTS.BaseURL)
			} else {
				ttsStatus = "not responding"
			}
		}
		fmt.Printf("  Piper TTS:     %s  [voice: %s]\n", ttsStatus, cfg.Voice.TTS.VoiceID)
	}

	fmt.Println()
	fmt.Println("Use 'her logs' to tail output, 'her stop' to stop.")

	return nil
}

// parseLaunchctlField extracts a value from launchctl print output.
// Lines look like: "    pid = 12345" or "    state = running".
func parseLaunchctlField(output, field string) string {
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, field+" = ") {
			parts := strings.SplitN(trimmed, " = ", 2)
			if len(parts) == 2 {
				return strings.TrimSpace(parts[1])
			}
		}
	}
	return ""
}

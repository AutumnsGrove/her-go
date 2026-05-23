package cmd

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"her/config"
	"her/procmgr"

	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show system service status",
	RunE:  runStatus,
}

func init() {
	rootCmd.AddCommand(statusCmd)
}

func runStatus(cmd *cobra.Command, args []string) error {
	label, err := loadServiceLabel()
	if err != nil {
		return err
	}

	mgr, err := procmgr.New(label)
	if err != nil {
		return err
	}

	if !mgr.IsManaged() {
		fmt.Println("Service is not running.")
		fmt.Println()
		fmt.Println("Run 'her setup' to install, or 'her start' to load.")
		return nil
	}

	fmt.Printf("Service: %s (via %s)\n", label, mgr.Name())
	fmt.Println()

	// Platform-specific status details.
	switch mgr.Name() {
	case "launchd":
		printLaunchdStatus(label)
	case "systemd":
		printSystemdStatus(label)
	}

	// Show sidecar status by hitting their health endpoints.
	cfg, err := config.Load("config.yaml")
	if err == nil {
		fmt.Println()
		fmt.Println("Sidecars:")
		printSidecarStatus(cfg)
	}

	fmt.Println()
	fmt.Println("Use 'her logs' to tail output, 'her stop' to stop.")

	return nil
}

func printLaunchdStatus(label string) {
	target := fmt.Sprintf("gui/%d/%s", getUID(), label)
	out, err := exec.Command("launchctl", "print", target).CombinedOutput()
	if err != nil {
		return
	}
	output := string(out)

	if val := parseLaunchctlField(output, "state"); val != "" {
		fmt.Printf("  State:       %s\n", val)
	}
	if val := parseLaunchctlField(output, "pid"); val != "" {
		fmt.Printf("  PID:         %s\n", val)
	}
	if val := parseLaunchctlField(output, "last exit code"); val != "" {
		fmt.Printf("  Last exit:   %s\n", val)
	}
	if val := parseLaunchctlField(output, "runs"); val != "" {
		fmt.Printf("  Run count:   %s\n", val)
	}
}

func printSystemdStatus(label string) {
	out, err := exec.Command("systemctl", "show", label,
		"--property=ActiveState,SubState,MainPID,ExecMainStartTimestamp,NRestarts",
		"--no-pager").CombinedOutput()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 || parts[1] == "" {
			continue
		}
		switch parts[0] {
		case "ActiveState":
			fmt.Printf("  State:       %s\n", parts[1])
		case "MainPID":
			fmt.Printf("  PID:         %s\n", parts[1])
		case "ExecMainStartTimestamp":
			fmt.Printf("  Started:     %s\n", parts[1])
		case "NRestarts":
			fmt.Printf("  Restarts:    %s\n", parts[1])
		}
	}
}

func printSidecarStatus(cfg *config.Config) {
	httpClient := &http.Client{Timeout: 2 * time.Second}

	sttEngine := cfg.Voice.STT.Engine
	if sttEngine == "" {
		sttEngine = "parakeet"
	}
	sttStatus := "disabled"
	if cfg.Voice.Enabled && cfg.Voice.STT.BaseURL != "" {
		if cfg.Voice.STT.Engine == config.STTEngineParakeet || cfg.Voice.STT.Engine == "" {
			sttURL := strings.TrimRight(cfg.Voice.STT.BaseURL, "/") + "/healthz"
			if resp, err := httpClient.Get(sttURL); err == nil {
				resp.Body.Close()
				sttStatus = fmt.Sprintf("running (%s)", cfg.Voice.STT.BaseURL)
			} else {
				sttStatus = "not responding"
			}
		} else {
			sttStatus = fmt.Sprintf("remote (%s)", cfg.Voice.STT.BaseURL)
		}
	}
	fmt.Printf("  STT (%s):  %s  [model: %s]\n", sttEngine, sttStatus, cfg.Voice.STT.Model)

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

// parseLaunchctlField extracts a value from launchctl print output.
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

// getUID returns the current user ID. Helper for launchd target paths.
func getUID() int {
	return os.Getuid()
}

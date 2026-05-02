package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"her/config"

	"github.com/spf13/cobra"
)

// voiceCmd is the parent command for voice-related testing.
// Subcommands: `her voice test` (or just `her voice` as shorthand).
var voiceCmd = &cobra.Command{
	Use:   "voice",
	Short: "Voice testing tools",
	Long:  "Launch the voice showroom — an interactive Gradio UI for testing TTS and STT sidecars.",
	RunE:  runVoiceShowroom,
}

var voiceTestCmd = &cobra.Command{
	Use:   "test",
	Short: "Open the voice showroom (TTS + STT testing UI)",
	RunE:  runVoiceShowroom,
}

func init() {
	rootCmd.AddCommand(voiceCmd)
	voiceCmd.AddCommand(voiceTestCmd)
}

// runVoiceShowroom launches the Gradio-based voice testing UI.
// It reads TTS/STT URLs from config so the showroom talks to the
// same sidecars the bot uses. If the sidecars aren't running, the
// showroom will start the TTS one automatically.
func runVoiceShowroom(cmd *cobra.Command, args []string) error {
	uvPath, err := exec.LookPath("uv")
	if err != nil {
		return fmt.Errorf("uv not found in PATH — install it with: curl -LsSf https://astral.sh/uv/install.sh | sh")
	}

	cfg, err := config.Load(cfgFile)
	if err != nil {
		// Non-fatal — fall back to defaults if config isn't available.
		cfg = &config.Config{}
	}

	ttsURL := cfg.Voice.TTS.BaseURL
	if ttsURL == "" {
		ttsURL = "http://localhost:8766"
	}
	sttURL := cfg.Voice.STT.BaseURL
	if sttURL == "" {
		sttURL = "http://localhost:8765"
	}

	script := filepath.Join("scripts", "voice_showroom.py")
	if _, err := os.Stat(script); err != nil {
		return fmt.Errorf("showroom script not found: %s", script)
	}

	fmt.Println("Launching voice showroom...")
	fmt.Printf("  TTS: %s\n", ttsURL)
	fmt.Printf("  STT: %s\n", sttURL)
	fmt.Println()

	// Run the Gradio app. uv handles dependencies automatically
	// from the inline script metadata (the /// script block).
	showroom := exec.Command(uvPath, "run", script,
		"--tts-url", ttsURL,
		"--stt-url", sttURL,
	)
	showroom.Stdout = os.Stdout
	showroom.Stderr = os.Stderr
	showroom.Stdin = os.Stdin

	return showroom.Run()
}

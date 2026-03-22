package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/spf13/cobra"
)

var (
	logsStderr bool
	logsLines  int
)

var logsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Tail the bot's log files",
	Long: `Shows live log output from the bot. By default tails stdout.

  her logs              — tail stdout (follows new output)
  her logs --stderr     — tail stderr instead
  her logs --lines 100  — show last 100 lines`,
	RunE: runLogs,
}

func init() {
	logsCmd.Flags().BoolVar(&logsStderr, "stderr", false, "tail stderr.log instead of stdout.log")
	logsCmd.Flags().IntVarP(&logsLines, "lines", "n", 50, "number of lines to show")
	rootCmd.AddCommand(logsCmd)
}

func runLogs(cmd *cobra.Command, args []string) error {
	workDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("could not determine working directory: %w", err)
	}

	logFile := "stdout.log"
	if logsStderr {
		logFile = "stderr.log"
	}
	logPath := filepath.Join(workDir, "logs", logFile)

	// Check the file exists before trying to tail it.
	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		return fmt.Errorf("log file not found: %s\nRun 'her setup' first to create the logs directory", logPath)
	}

	fmt.Printf("Tailing %s (last %d lines, Ctrl+C to stop)\n\n", logPath, logsLines)

	// tail -f follows the file as new lines are written.
	// The -n flag shows the last N lines before following.
	tailCmd := exec.Command("tail", "-f", "-n", strconv.Itoa(logsLines), logPath)
	tailCmd.Stdout = os.Stdout
	tailCmd.Stderr = os.Stderr

	// Run blocks until the user hits Ctrl+C.
	return tailCmd.Run()
}

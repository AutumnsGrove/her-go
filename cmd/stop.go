package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the launchd service",
	RunE:  runStop,
}

func init() {
	rootCmd.AddCommand(stopCmd)
}

func runStop(cmd *cobra.Command, args []string) error {
	botName, err := loadBotName()
	if err != nil {
		return err
	}
	// Use the modern launchctl bootout command. The old "launchctl unload"
	// is deprecated and can silently misbehave on modern macOS.
	label := serviceLabel(botName)
	if err := launchdBootout(label); err != nil {
		fmt.Printf("Failed to stop service: %v\n", err)
		return err
	}

	fmt.Println("Service stopped.")
	return nil
}

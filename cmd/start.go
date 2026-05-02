package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the launchd service",
	Long:  "Starts her-go as a launchd service. If setup hasn't been run yet, runs it automatically first.",
	RunE:  runStart,
}

func init() {
	rootCmd.AddCommand(startCmd)
}

func runStart(cmd *cobra.Command, args []string) error {
	botName, err := loadBotName()
	if err != nil {
		return err
	}
	dest, err := plistPath(botName)
	if err != nil {
		return err
	}

	// Check if the plist exists. If not, run setup first.
	if _, err := os.Stat(dest); os.IsNotExist(err) {
		fmt.Println("Service not set up yet. Running setup first...")
		fmt.Println()
		if err := runSetup(cmd, args); err != nil {
			return fmt.Errorf("setup failed: %w", err)
		}
		// Setup already loads the service, so we're done.
		return nil
	}

	// Use the modern launchctl bootstrap command. launchdBootstrap handles
	// the "already loaded" case automatically (bootout + retry).
	label := serviceLabel(botName)
	if err := launchdBootstrap(dest, label); err != nil {
		fmt.Printf("Failed to start service: %v\n", err)
		return nil
	}

	fmt.Println("Service started.")
	return nil
}

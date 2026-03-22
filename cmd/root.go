// Package cmd contains all CLI subcommands for her-go.
// Built with Cobra — each file defines one command.
package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// cfgFile holds the path to the config file, set via --config / -c.
var cfgFile string

// rootCmd is the base command — "her" with no subcommand shows help.
var rootCmd = &cobra.Command{
	Use:   "her",
	Short: "Mira — personal companion bot",
	Long:  "Mira is a personal companion bot that runs on Telegram.\nUse subcommands to run, manage, and monitor the service.",
	// No RunE — running bare "her" just prints help.
}

// Execute runs the root command. Called from main.go.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	// Persistent flag available to all subcommands.
	rootCmd.PersistentFlags().StringVarP(&cfgFile, "config", "c", "config.yaml", "path to config file")
}

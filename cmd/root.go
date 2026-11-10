// Package cmd contains all CLI subcommands for her-go.
// Built with Cobra — each file defines one command.
package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"her/agent_engine"
	"her/config"
	"her/llm"
)

// cfgFile holds the path to the config file, set via --config / -c.
var cfgFile string

// rootCmd is the base command — "her" with no subcommand shows help.
var rootCmd = &cobra.Command{
	Use:   "her",
	Short: "her-go — personal companion bot",
	Long:  "her-go is a personal companion bot that runs on Telegram.\nUse subcommands to run, manage, and monitor the service.",
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

	// PersistentPreRun updates the CLI description with the configured
	// bot name. Runs before any subcommand, so --help always shows
	// the right name. Errors are swallowed — if config can't load,
	// the default description is fine.
	rootCmd.PersistentPreRun = func(cmd *cobra.Command, args []string) {
		if cfg, err := config.Load(cfgFile); err == nil {
			// Pick the transport implementation once, before any client is
			// built. llm.SetBackend is process-wide by design: every client in a
			// run must speak the same dialect or the logs and metrics mix.
			llm.SetBackend(cfg.LLM.Backend)
			agent_engine.SetLoopBackend(cfg.Engine.Backend, cfg.Engine.BackendOverrides)

			rootCmd.Short = cfg.Identity.Her + " — personal companion bot"
			rootCmd.Long = cfg.Identity.Her + " is a personal companion bot that runs on Telegram.\nUse subcommands to run, manage, and monitor the service."
		}
	}
}

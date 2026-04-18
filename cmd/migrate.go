package cmd

import (
	"fmt"

	"her/config"
	"her/memory"

	"github.com/spf13/cobra"
)

var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "One-time migration: copy facts → memories",
	Long: `Copies all data from the legacy 'facts' and 'fact_links' tables into
the new 'memories' and 'memory_links' tables. Run this once after upgrading.

Embeddings in vec_memories will be rebuilt automatically on the next 'her run'
via the startup backfill — no extra step needed.

Safe to run multiple times (INSERT OR IGNORE skips already-copied rows).`,
	RunE: runMigrate,
}

func init() {
	rootCmd.AddCommand(migrateCmd)
}

func runMigrate(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	store, err := memory.NewStore(cfg.Memory.DBPath, cfg.Embed.Dimension)
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer store.Close()

	fmt.Println("Migrating facts → memories...")

	memoriesCopied, linksCopied, err := store.MigrateFromLegacyFacts()
	if err != nil {
		return fmt.Errorf("migration failed: %w", err)
	}

	fmt.Printf("  ✓ %d memories copied\n", memoriesCopied)
	fmt.Printf("  ✓ %d memory links copied\n", linksCopied)
	fmt.Println("Done. Run 'her run' — vec_memories will be rebuilt on startup.")
	return nil
}

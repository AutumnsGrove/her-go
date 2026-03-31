package cmd

import (
	"fmt"

	"her/config"
	"her/memory"

	"github.com/spf13/cobra"
)

var relinkCmd = &cobra.Command{
	Use:   "relink",
	Short: "Backfill Zettelkasten links for existing facts",
	Long: `One-time backfill: scans all active facts that have embeddings and
creates links between similar ones using the same auto-link logic that
runs on new facts. Safe to run multiple times — duplicate links are
silently skipped (INSERT OR IGNORE).

Run this once after enabling fact linking (auto_link_count > 0).`,
	RunE: runRelink,
}

func init() {
	rootCmd.AddCommand(relinkCmd)
}

func runRelink(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	store, err := memory.NewStore(cfg.Memory.DBPath, cfg.Embed.Dimension)
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer store.Close()
	store.AutoLinkCount = cfg.Memory.AutoLinkCount
	store.AutoLinkThreshold = cfg.Memory.AutoLinkThreshold

	if store.AutoLinkCount == 0 {
		fmt.Println("auto_link_count is 0 — linking is disabled. Set it in config.yaml first.")
		return nil
	}

	// Load all active facts — we need the ones with embeddings.
	facts, err := store.AllActiveFacts()
	if err != nil {
		return fmt.Errorf("loading facts: %w", err)
	}

	// Filter to facts that have a tag embedding (needed for KNN search).
	var linkable []memory.Fact
	for _, f := range facts {
		if len(f.Embedding) > 0 {
			linkable = append(linkable, f)
		}
	}

	if len(linkable) == 0 {
		fmt.Println("No facts with embeddings found. Run 'her retag' first.")
		return nil
	}

	fmt.Printf("Found %d facts with embeddings. Linking (max %d neighbors, threshold %.2f)...\n",
		len(linkable), store.AutoLinkCount, store.AutoLinkThreshold)

	linked := 0
	errors := 0
	for i, f := range linkable {
		if err := store.AutoLinkFact(f.ID, f.Embedding); err != nil {
			fmt.Printf("  Error linking fact #%d: %v\n", f.ID, err)
			errors++
			continue
		}
		// Progress indicator every 25 facts (or at the end).
		if (i+1)%25 == 0 || i == len(linkable)-1 {
			fmt.Printf("  Processed %d/%d facts\n", i+1, len(linkable))
		}
		linked++
	}

	// Count total links created.
	totalLinks, err := store.CountFactLinks()
	if err != nil {
		fmt.Printf("\nDone! Processed %d facts, %d errors.\n", linked, errors)
	} else {
		fmt.Printf("\nDone! Processed %d facts, %d errors. Total links in graph: %d\n", linked, errors, totalLinks)
	}

	return nil
}

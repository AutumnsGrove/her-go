package cmd

import (
	"fmt"
	"strings"

	"her/classifier"
	"her/config"
	"her/embed"
	"her/llm"
	"her/memory"

	"github.com/spf13/cobra"
)

var splitMemoriesCmd = &cobra.Command{
	Use:   "split-memories",
	Short: "Run the SPLIT classifier over existing memories to atomize packed ones",
	Long: `Runs each active memory through the classifier. When the classifier
returns SPLIT, the original memory is deactivated and replaced with focused,
atomic sub-memories — one idea each.

Use --dry-run first to preview what would be split without touching the DB.
Only memories above 120 characters are checked (shorter ones are almost always atomic).`,
	RunE: runSplitMemories,
}

var splitDryRun bool
var splitMinLength int
var splitExclude []int64

func init() {
	rootCmd.AddCommand(splitMemoriesCmd)
	splitMemoriesCmd.Flags().BoolVar(&splitDryRun, "dry-run", false, "Preview splits without writing to the database")
	splitMemoriesCmd.Flags().IntVar(&splitMinLength, "min-length", 120, "Only check memories at or above this character length")
	splitMemoriesCmd.Flags().Int64SliceVar(&splitExclude, "exclude", nil, "Memory IDs to skip (comma-separated, e.g. --exclude 18,42)")
}

func runSplitMemories(cmd *cobra.Command, args []string) error {
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

	if cfg.Classifier.Model == "" {
		return fmt.Errorf("no classifier model configured (classifier.model in config.yaml)")
	}

	maxTokens := cfg.Classifier.MaxTokens
	if maxTokens == 0 {
		maxTokens = 256
	}
	classifierClient := llm.NewClient(
		cfg.LLM.BaseURL, cfg.LLM.APIKey,
		cfg.Classifier.Model, cfg.Classifier.Temperature, maxTokens,
	)

	var embedClient *embed.Client
	if cfg.Embed.BaseURL != "" && cfg.Embed.Model != "" {
		embedClient = embed.NewClient(cfg.Embed.BaseURL, cfg.Embed.Model, cfg.Embed.APIKey, cfg.Embed.Dimension)
	}

	memories, err := store.AllActiveMemories()
	if err != nil {
		return fmt.Errorf("loading memories: %w", err)
	}

	// Build exclude set for fast lookup.
	excluded := make(map[int64]bool, len(splitExclude))
	for _, id := range splitExclude {
		excluded[id] = true
	}

	// Filter to memories above the minimum length threshold, skipping excluded IDs.
	var candidates []memory.Memory
	for _, m := range memories {
		if excluded[m.ID] {
			fmt.Printf("  SKIP  #%d [excluded]\n", m.ID)
			continue
		}
		if len(m.Content) >= splitMinLength {
			candidates = append(candidates, m)
		}
	}

	if len(candidates) == 0 {
		fmt.Printf("No memories at or above %d characters. Nothing to do.\n", splitMinLength)
		return nil
	}

	if splitDryRun {
		fmt.Printf("DRY RUN — checking %d memories (>= %d chars), no changes will be written.\n\n", len(candidates), splitMinLength)
	} else {
		fmt.Printf("Checking %d memories (>= %d chars)...\n\n", len(candidates), splitMinLength)
	}

	var splitCount, savedCount, skippedCount int

	for _, m := range candidates {
		verdict := classifier.Check(classifierClient, "memory", m.Content, nil)

		if verdict.Type != "SPLIT" || len(verdict.Splits) < 2 {
			skippedCount++
			fmt.Printf("  SAVE  #%d [%s] %s\n", m.ID, m.Category, truncateMemory(m.Content, 80))
			continue
		}

		splitCount++
		fmt.Printf("  SPLIT #%d [%s] %s\n", m.ID, m.Category, truncateMemory(m.Content, 80))
		if verdict.Reason != "" {
			fmt.Printf("        reason: %s\n", verdict.Reason)
		}
		for i, sub := range verdict.Splits {
			fmt.Printf("        → [%d] %s\n", i+1, sub)
		}

		if splitDryRun {
			continue
		}

		// Save each sub-memory with its own embedding.
		var newIDs []int64
		for _, sub := range verdict.Splits {
			sub = strings.TrimSpace(sub)
			if sub == "" {
				continue
			}

			var vec []float32
			if embedClient != nil {
				vec, err = embedClient.Embed(sub)
				if err != nil {
					fmt.Printf("        ⚠ embedding failed for sub-memory: %v\n", err)
				}
			}

			id, err := store.SaveMemory(sub, m.Category, m.Subject, 0, m.Importance, vec, vec, "", "")
			if err != nil {
				fmt.Printf("        ⚠ save failed: %v\n", err)
				continue
			}

			if embedClient != nil && len(vec) > 0 {
				_ = store.AutoLinkMemory(id, vec)
			}

			newIDs = append(newIDs, id)
			savedCount++
			fmt.Printf("        ✓ saved ID=%d: %s\n", id, truncateMemory(sub, 70))
		}

		// Deactivate the original only if we successfully saved sub-memories.
		if len(newIDs) > 0 {
			if err := store.DeactivateMemory(m.ID); err != nil {
				fmt.Printf("        ⚠ failed to deactivate original ID=%d: %v\n", m.ID, err)
			} else {
				fmt.Printf("        ✓ deactivated original ID=%d\n", m.ID)
			}
		}
	}

	fmt.Println()
	if splitDryRun {
		fmt.Printf("Dry run complete: %d would be split, %d would be left as-is.\n", splitCount, skippedCount)
	} else {
		fmt.Printf("Done: %d memories split into %d sub-memories, %d left as-is.\n", splitCount, savedCount, skippedCount)
	}
	return nil
}

func truncateMemory(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

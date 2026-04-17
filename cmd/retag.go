package cmd

import (
	"encoding/json"
	"fmt"
	"strings"

	"her/config"
	"her/embed"
	"her/llm"
	"her/memory"

	"github.com/spf13/cobra"
)

var retagCmd = &cobra.Command{
	Use:   "retag",
	Short: "Generate topic tags for existing facts and re-embed them",
	Long: `One-time backfill: uses the LLM to generate topic tags for facts
that don't have them yet, then re-embeds using the tags so semantic
search works by topic rather than surface-level word overlap.

Run this once after upgrading to tag-based embeddings.`,
	RunE: runRetag,
}

func init() {
	rootCmd.AddCommand(retagCmd)
}

func runRetag(cmd *cobra.Command, args []string) error {
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

	// Load all active facts that need tagging.
	facts, err := store.AllActiveFacts()
	if err != nil {
		return fmt.Errorf("loading facts: %w", err)
	}

	// Filter to facts without tags.
	var untagged []memory.Fact
	for _, f := range facts {
		if f.Tags == "" {
			untagged = append(untagged, f)
		}
	}

	if len(untagged) == 0 {
		fmt.Println("All facts already have tags. Nothing to do.")
		return nil
	}

	fmt.Printf("Found %d facts without tags. Generating...\n", len(untagged))

	// Create LLM client for tag generation.
	llmClient := llm.NewClient(
		cfg.LLM.BaseURL, cfg.LLM.APIKey,
		cfg.Chat.Model, 0.3, 256, // low temp, short output
	)
	if cfg.Chat.Fallback != nil {
		llmClient.WithFallback(cfg.Chat.Fallback.Model, cfg.Chat.Fallback.Temperature, cfg.Chat.Fallback.MaxTokens)
	}

	// Create embedding client for re-embedding.
	var embedClient *embed.Client
	if cfg.Embed.BaseURL != "" && cfg.Embed.Model != "" {
		embedClient = embed.NewClient(cfg.Embed.BaseURL, cfg.Embed.Model, cfg.Embed.APIKey, cfg.Embed.Dimension)
	}

	// Process facts in batches of 10 to reduce LLM calls.
	batchSize := 10
	for i := 0; i < len(untagged); i += batchSize {
		end := i + batchSize
		if end > len(untagged) {
			end = len(untagged)
		}
		batch := untagged[i:end]

		tags, err := generateTagsBatch(llmClient, batch)
		if err != nil {
			fmt.Printf("  Error generating tags for batch %d-%d: %v\n", i, end-1, err)
			continue
		}

		for j, f := range batch {
			if j >= len(tags) {
				break
			}
			t := tags[j]
			if t == "" {
				continue
			}

			// Save tags to DB.
			if err := store.UpdateFactTags(f.ID, t); err != nil {
				fmt.Printf("  Error saving tags for fact #%d: %v\n", f.ID, err)
				continue
			}

			// Re-embed using tags.
			if embedClient != nil {
				vec, err := embedClient.Embed(t)
				if err != nil {
					fmt.Printf("  Error embedding tags for fact #%d: %v\n", f.ID, err)
					continue
				}
				// Pass nil for embeddingText — retag only refreshes the tag
				// embedding (used for vec_facts KNN search). Text embeddings
				// are populated lazily by checkDuplicate when needed.
				if err := store.UpdateFactEmbedding(f.ID, vec, nil); err != nil {
					fmt.Printf("  Error updating embedding for fact #%d: %v\n", f.ID, err)
					continue
				}
			}

			fmt.Printf("  #%d [%s] tags: %s\n", f.ID, f.Category, t)
		}
	}

	fmt.Println("Done! All facts tagged and re-embedded.")
	return nil
}

// generateTagsBatch asks the LLM to generate topic tags for a batch of facts
// in a single call. Returns one tag string per fact.
func generateTagsBatch(client *llm.Client, facts []memory.Fact) ([]string, error) {
	// Build the prompt with all facts.
	var prompt strings.Builder
	prompt.WriteString("Generate comma-separated topic tags for each fact below. Tags should describe WHAT the fact is about — the topics, themes, and contexts where this fact would be relevant in conversation. Be specific, not generic. Return a JSON array of strings, one per fact.\n\n")

	for i, f := range facts {
		prompt.WriteString(fmt.Sprintf("%d. [%s, %s] %s\n", i+1, f.Category, f.Subject, f.Fact))
	}

	prompt.WriteString(fmt.Sprintf("\nReturn ONLY a JSON array of %d strings, one per fact. Example: [\"mental health, burnout, coping, energy\", \"programming, go, backend, projects\"]", len(facts)))

	resp, err := client.ChatCompletion([]llm.ChatMessage{
		{Role: "user", Content: prompt.String()},
	})
	if err != nil {
		return nil, fmt.Errorf("LLM error: %w", err)
	}

	// Parse the JSON array from the response.
	content := strings.TrimSpace(resp.Content)
	// Strip markdown code fences if present.
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)

	var tags []string
	if err := json.Unmarshal([]byte(content), &tags); err != nil {
		return nil, fmt.Errorf("failed to parse tags JSON: %w (response: %s)", err, content[:min(len(content), 200)])
	}

	return tags, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

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
	Short: "Generate topic tags for existing memories and re-embed them",
	Long: `One-time backfill: uses the LLM to generate topic tags for memories
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

	// Load all active memories that need tagging.
	memories, err := store.AllActiveMemories()
	if err != nil {
		return fmt.Errorf("loading memories: %w", err)
	}

	// Filter to memories without tags.
	var untagged []memory.Memory
	for _, m := range memories {
		if m.Tags == "" {
			untagged = append(untagged, m)
		}
	}

	if len(untagged) == 0 {
		fmt.Println("All memories already have tags. Nothing to do.")
		return nil
	}

	fmt.Printf("Found %d memories without tags. Generating...\n", len(untagged))

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

	// Process memories in batches of 10 to reduce LLM calls.
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

		for j, m := range batch {
			if j >= len(tags) {
				break
			}
			t := tags[j]
			if t == "" {
				continue
			}

			// Save tags to DB.
			if err := store.UpdateMemoryTags(m.ID, t); err != nil {
				fmt.Printf("  Error saving tags for memory #%d: %v\n", m.ID, err)
				continue
			}

			// Re-embed using tags.
			if embedClient != nil {
				vec, err := embedClient.Embed(t)
				if err != nil {
					fmt.Printf("  Error embedding tags for memory #%d: %v\n", m.ID, err)
					continue
				}
				// Pass nil for embeddingText — retag only refreshes the tag
				// embedding (used for vec_memories KNN search). Text embeddings
				// are populated lazily by checkDuplicate when needed.
				if err := store.UpdateMemoryEmbedding(m.ID, vec, nil); err != nil {
					fmt.Printf("  Error updating embedding for memory #%d: %v\n", m.ID, err)
					continue
				}
			}

			fmt.Printf("  #%d [%s] tags: %s\n", m.ID, m.Category, t)
		}
	}

	fmt.Println("Done! All memories tagged and re-embedded.")
	return nil
}

// generateTagsBatch asks the LLM to generate topic tags for a batch of memories
// in a single call. Returns one tag string per memory.
func generateTagsBatch(client *llm.Client, memories []memory.Memory) ([]string, error) {
	// Build the prompt with all memories.
	var prompt strings.Builder
	prompt.WriteString("Generate comma-separated topic tags for each memory below. Tags should describe WHAT the memory is about — the topics, themes, and contexts where this memory would be relevant in conversation. Be specific, not generic. Return a JSON array of strings, one per memory.\n\n")

	for i, m := range memories {
		prompt.WriteString(fmt.Sprintf("%d. [%s, %s] %s\n", i+1, m.Category, m.Subject, m.Content))
	}

	prompt.WriteString(fmt.Sprintf("\nReturn ONLY a JSON array of %d strings, one per memory. Example: [\"mental health, burnout, coping, energy\", \"programming, go, backend, projects\"]", len(memories)))

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

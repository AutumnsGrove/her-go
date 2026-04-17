package cmd

import (
	"fmt"
	"strings"

	"her/layers"
	"her/config"
	"her/embed"
	"her/logger"
	"her/memory"

	"github.com/spf13/cobra"
)

var shapeCmd = &cobra.Command{
	Use:   "shape [agent|chat]",
	Short: "Show the shape of each model's prompt",
	Long: `Displays a breakdown of what fills each model's context window —
every layer, its token count, and metadata. Uses live data from the
database when available, falling back to estimates when not.

This is the observability tool for understanding why your models
are using the tokens they're using. The layers shown here are the
exact same layers used at runtime — they share the same registry,
so this output can never drift out of sync with the actual code.

Examples:
  her shape          # show both agent and chat
  her shape agent    # show only agent prompt shape
  her shape chat     # show only chat prompt shape`,
	RunE: runShape,
}

func init() {
	rootCmd.AddCommand(shapeCmd)
}

func runShape(cmd *cobra.Command, args []string) error {
	// Suppress info-level logs from subsystems (fact filtering, weather, etc.)
	// so the shape output stays clean — just the table, no noise.
	logger.Quiet()

	showAgent := true
	showChat := true

	if len(args) > 0 {
		switch args[0] {
		case "agent":
			showChat = false
		case "chat":
			showAgent = false
		default:
			return fmt.Errorf("unknown stream %q — use 'agent' or 'chat'", args[0])
		}
	}

	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// Open the database for live data (facts, messages, mood, etc.).
	// If it doesn't exist, we run with nil store — layers handle this
	// gracefully and return empty/estimated results.
	var store *memory.Store
	store, err = memory.NewStore(cfg.Memory.DBPath, cfg.Embed.Dimension)
	if err != nil {
		fmt.Printf("  (no database found at %s — using estimates)\n\n", cfg.Memory.DBPath)
	} else {
		defer store.Close()
	}

	// Set up embedding client if configured.
	var embedClient *embed.Client
	if cfg.Embed.BaseURL != "" {
		embedClient = embed.NewClient(cfg.Embed.BaseURL, cfg.Embed.Model, cfg.Embed.APIKey, cfg.Embed.Dimension)
	}

	// Build the layer context with whatever live data we have.
	ctx := &layers.LayerContext{
		Store:       store,
		Cfg:         cfg,
		EmbedClient: embedClient,
	}

	// If we have a store, populate with real data from the last conversation.
	if store != nil {
		// Get recent messages for the agent context.
		msgs, err := store.RecentMessages("", cfg.Memory.RecentMessages)
		if err == nil {
			ctx.RecentMessages = msgs
		}

		// Get semantically relevant facts (use a generic query).
		if embedClient != nil {
			queryVec, err := embedClient.Embed("general conversation")
			if err == nil {
				facts, err := store.SemanticSearch(queryVec, cfg.Memory.MaxFactsInContext)
				if err == nil {
					ctx.RelevantFacts = facts
				}
			}
		}

		// Get the latest conversation summary.
		summary, _, err := store.LatestSummary("", "chat")
		if err == nil {
			ctx.ConversationSummary = summary
		}

		// Get the latest agent action summary and recent actions.
		agentSummary, _, err := store.LatestSummary("", "agent")
		if err == nil {
			ctx.AgentActionSummary = agentSummary
		}
		agentActions, err := store.RecentAgentActions("", 30)
		if err == nil {
			ctx.RecentAgentActions = agentActions
		}
	}

	// Use a mock user message for shape estimation.
	ctx.ScrubbedUserMessage = "(sample message for shape estimation)"

	if showAgent {
		printStreamShape("Agent", cfg.Agent.Model, layers.StreamAgent, ctx, cfg)
	}

	if showAgent && showChat {
		fmt.Println()
	}

	if showChat {
		printStreamShape("Chat", cfg.LLM.Model, layers.StreamChat, ctx, cfg)
	}

	return nil
}

// printStreamShape renders the shape table for one stream (agent or chat).
func printStreamShape(name, model string, stream layers.Stream, ctx *layers.LayerContext, cfg *config.Config) {
	results := layers.Shape(stream, ctx)

	// Header.
	header := fmt.Sprintf(" %s Prompt (%s)", name, model)
	width := 54
	fmt.Println(strings.Repeat("─", width))
	fmt.Printf(" %s\n", header)
	fmt.Println(strings.Repeat("─", width))

	// Layer rows.
	var totalTokens int
	for _, r := range results {
		tokens := r.Tokens
		totalTokens += tokens

		label := r.Name
		if r.Detail != "" {
			label = fmt.Sprintf("%s (%s)", r.Name, r.Detail)
		}

		if r.Content == "" && tokens > 0 {
			// Overhead layer (system prompt, tool schemas) — shown dimmed.
			fmt.Printf("  %-38s %6d tokens  ○\n", label, tokens)
		} else if r.Content == "" && tokens == 0 {
			// Skipped layer — show that it exists but was empty.
			fmt.Printf("  %-38s      — skipped\n", label)
		} else {
			fmt.Printf("  %-38s %6d tokens\n", label, tokens)
		}
	}

	// Total + budget.
	fmt.Println(strings.Repeat("─", width))
	fmt.Printf("  %-38s %6d tokens\n", "TOTAL", totalTokens)

	// Show budget and headroom. For chat, the effective budget is
	// scaffolding + max_history_tokens (what compaction actually uses).
	// For agent, it's the agent_context_budget directly.
	var budget int
	if stream == layers.StreamAgent {
		budget = cfg.Memory.AgentContextBudget
	} else {
		// Derive total budget: current scaffolding + history budget.
		// This reflects what the prompt will look like at compaction threshold.
		scaffolding := totalTokens // current shape IS the scaffolding (no real history in shape mode)
		historyBudget := cfg.Memory.MaxHistoryTokens
		if historyBudget <= 0 {
			historyBudget = 3000
		}
		budget = scaffolding + historyBudget
	}

	if budget > 0 {
		headroom := budget - totalTokens
		status := "✓"
		if headroom <= 0 {
			status = "⚠ scaffolding alone exceeds budget"
		}
		fmt.Printf("  Budget: %d  Headroom: %d  %s\n",
			budget, headroom, status)
	}
	fmt.Println(strings.Repeat("─", width))
}

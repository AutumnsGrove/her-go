package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"her/config"
	"her/llm"
	"her/memory"
	"her/retry"

	"github.com/spf13/cobra"
)

var backfillHooksCmd = &cobra.Command{
	Use:   "backfill-hooks",
	Short: "Generate retrieval hooks for memories saved before feat/memory-hooks",
	Long: `One-off command: finds every active memory with no hook yet and generates
one via the memory agent's model, using the same "<label> — <trigger condition>"
convention save_memory now requires at write time. See
docs/plans/PLAN-memory-hooks-dual-recall.md.

Safe to run more than once — MemoriesWithoutHooks only returns memories that
still lack a hook, so an interrupted run just picks up where it left off.`,
	RunE: runBackfillHooks,
}

var backfillDryRun bool

func init() {
	rootCmd.AddCommand(backfillHooksCmd)
	backfillHooksCmd.Flags().BoolVar(&backfillDryRun, "dry-run", false, "Preview generated hooks without writing to the database")
}

// hookPromptTemplate mirrors the convention documented in save_memory's
// `hook` parameter (tools/save_memory/tool.yaml) — kept in sync by hand
// since this is a one-off backfill, not a hot path worth centralizing.
const hookPromptTemplate = `Write a single-line retrieval hook for this memory, in the format:
<short label> — <trigger condition>

The label is 2-5 words, no punctuation beyond spaces, sentence case (capitalize
only the first word and proper nouns — not "Digital Legacy", write "Digital
legacy"). The label must be SPECIFIC enough to distinguish this memory from
others on a similar topic — avoid generic labels like "steady space" or
"autumn's struggles" that could describe many different memories. The trigger
condition describes WHEN this memory matters — not a restatement of the
memory itself.

Example: memory = "%s told Mira about an inside joke: if a stranger ever calls
her 'babe' or 'sweetheart' while she's out, she wants Mira to jokingly call her
'Donette' for the rest of the night, like she's a mob boss's wife."
hook = "Donette bit — call her this when a stranger uses a pet name on her in public"

Memory to write a hook for:
%s

Reply with ONLY the hook line, nothing else.`

func runBackfillHooks(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	store, err := memory.NewStore(cfg.Memory.DBPath, cfg.Embed.Dimension)
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer store.Close()

	if cfg.MemoryAgent.Model == "" {
		return fmt.Errorf("no memory agent model configured (memory_agent.model in config.yaml)")
	}
	maxTokens := cfg.MemoryAgent.MaxTokens
	if maxTokens == 0 || maxTokens > 200 {
		maxTokens = 200 // a hook is one short line — no need for the agent's usual budget
	}
	client := llm.NewClient(
		cfg.OpenRouter.BaseURL, cfg.OpenRouter.APIKey,
		cfg.MemoryAgent.Model, cfg.MemoryAgent.Temperature, maxTokens,
	)
	// Disable reasoning: MiMo is a hybrid reasoning model, and every
	// production client explicitly controls this (cmd/run.go, cmd/sim.go).
	// Without it, a hybrid model can spend the whole token budget on hidden
	// reasoning tokens and leave nothing for the actual hook — which is
	// exactly what happened in initial testing (53/64 came back empty).
	reasoningDisabled := false
	client.WithReasoning(&llm.ReasoningControl{Enabled: &reasoningDisabled})

	targets, err := store.MemoriesWithoutHooks()
	if err != nil {
		return fmt.Errorf("loading memories without hooks: %w", err)
	}
	if len(targets) == 0 {
		fmt.Println("No memories need hooks. Nothing to do.")
		return nil
	}

	if backfillDryRun {
		fmt.Printf("DRY RUN — generating hooks for %d memories, no changes will be written.\n\n", len(targets))
	} else {
		fmt.Printf("Generating hooks for %d memories...\n\n", len(targets))
	}

	var hooked, failed int
	for _, m := range targets {
		hook, err := generateHook(client, cfg.Identity.User, m.Content)
		if err != nil {
			failed++
			fmt.Printf("  #%-4d ⚠ generation failed: %v\n", m.ID, err)
			continue
		}

		slug, err := store.UniqueSlug(memory.DeriveSlug(hook))
		if err != nil {
			failed++
			fmt.Printf("  #%-4d ⚠ slug derivation failed: %v\n", m.ID, err)
			continue
		}

		fmt.Printf("  #%-4d %s\n         → %s [%s]\n", m.ID, truncateMemory(m.Content, 70), hook, slug)

		if backfillDryRun {
			continue
		}
		if err := store.SetMemoryHook(m.ID, hook, slug); err != nil {
			failed++
			fmt.Printf("         ⚠ save failed: %v\n", err)
			continue
		}
		hooked++
	}

	fmt.Printf("\nDone. %d hooked, %d failed.\n", hooked, failed)
	return nil
}

// generateHook asks the memory agent's model for a single hook line and
// strips the surrounding quotes/whitespace models tend to add even when
// told not to.
func generateHook(client *llm.Client, userName, content string) (string, error) {
	prompt := fmt.Sprintf(hookPromptTemplate, userName, content)
	var hook string
	err := retry.Do(context.Background(), retry.Config{
		MaxAttempts: 3,
		Backoff:     retry.Linear,
		InitialWait: 2 * time.Second,
	}, func() error {
		resp, err := client.ChatCompletion([]llm.ChatMessage{
			{Role: "user", Content: prompt},
		})
		if err != nil {
			return err
		}
		h := strings.TrimSpace(resp.Content)
		h = strings.Trim(h, `"'`)
		if h == "" {
			return fmt.Errorf("model returned an empty hook")
		}
		hook = h
		return nil
	})
	if err != nil {
		return "", err
	}
	return hook, nil
}

package cmd

import (
	"fmt"
	"os"

	"her/config"
	"her/embed"
	"her/memory"

	"github.com/spf13/cobra"
)

// reembedCmd rebuilds the vector index after the embedding model changes.
//
// Background: `vec_memories` and `vec_moods` are sqlite-vec virtual tables whose
// width is fixed at creation time (`float[N]`). Switching embedding models means
// switching dimensions, and `CREATE ... IF NOT EXISTS` will happily keep the old
// table — so the store now refuses to open on a mismatch (see memory/vec.go) and
// points here instead.
//
// This command is deliberately manual and one-shot: it rewrites every row's
// vectors, which costs real money on a remote embedding provider and takes time
// on a Pi. It is safe to re-run (idempotent, resumable), and `--dry-run` shows
// the size of the job before spending anything.
var reembedCmd = &cobra.Command{
	Use:   "reembed",
	Short: "Rebuild the vector index at the configured embedding dimension",
	Long: `Recomputes every stored embedding (memories and mood entries) using the
model in config.yaml's embed: block, and rebuilds vec_memories / vec_moods at
that model's dimension.

Run this once after changing embed.dimension (for example when moving from a
local 768-dimension model to a 1024-dimension remote one). The store refuses to
open with a mismatched index, so this is the documented fix.

Existing memories and mood entries are the source of truth; their text is
re-embedded, not regenerated. Before anything is dropped it makes one test
embedding, so a bad key, an outage, or an embed.dimension that does not match
the model stops the run with the old index intact.

Safe to re-run: rows that already hold vectors at the configured dimension are
skipped, so an interrupted run resumes instead of paying again. Use --force to
recompute every row regardless — needed when the model changed but the
dimension did not.

Exits non-zero if any row failed to embed, so a script or deploy notices.

Examples:
  her reembed --dry-run   # count rows and estimate the cost, change nothing
  her reembed             # rebuild the index if needed and re-embed what is stale
  her reembed --force     # rebuild and recompute every row`,
	RunE: runReembed,
}

func init() {
	reembedCmd.Flags().Bool("dry-run", false, "report row counts and estimated cost without changing anything")
	reembedCmd.Flags().Bool("force", false, "recompute every row even if it already has vectors at the configured dimension")
	rootCmd.AddCommand(reembedCmd)
}

func runReembed(cmd *cobra.Command, args []string) error {
	dryRun, err := cmd.Flags().GetBool("dry-run")
	if err != nil {
		return fmt.Errorf("reading --dry-run: %w", err)
	}

	force, err := cmd.Flags().GetBool("force")
	if err != nil {
		return fmt.Errorf("reading --force: %w", err)
	}

	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	if cfg.Embed.BaseURL == "" || cfg.Embed.Model == "" {
		return fmt.Errorf("embed.base_url and embed.model must be set in config.yaml")
	}
	if cfg.Embed.Dimension <= 0 {
		return fmt.Errorf("embed.dimension must be a positive number of dimensions")
	}

	// AllowVecDimensionMismatch is the whole reason the open option exists:
	// without it the store would (correctly) refuse to open the very database
	// this command is here to repair.
	store, err := memory.NewStore(cfg.Memory.DBPath, cfg.Embed.Dimension, memory.AllowVecDimensionMismatch())
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer store.Close()

	memories, err := store.AllActiveMemories()
	if err != nil {
		return fmt.Errorf("counting memories: %w", err)
	}

	embedClient := embed.NewClient(cfg.Embed.BaseURL, cfg.Embed.Model, cfg.Embed.APIKey, cfg.Embed.Dimension)

	fmt.Printf("Database:  %s\n", cfg.Memory.DBPath)
	fmt.Printf("Model:     %s (%d dims)\n", cfg.Embed.Model, cfg.Embed.Dimension)
	fmt.Printf("Memories:  %d (2 vectors each: tags + text)\n", len(memories))

	if existing, err := store.VecIndexDimensions(); err == nil {
		for table, dim := range existing {
			if dim == 0 {
				fmt.Printf("%-10s not created yet\n", table+":")
			} else {
				fmt.Printf("%-10s float[%d]\n", table+":", dim)
			}
		}
	}

	if dryRun {
		// Rough token estimate: ~4 characters per token is the usual rule of
		// thumb for English. The figure is a sanity check, not a bill — the
		// provider reports the real cost on every response.
		chars := 0
		for _, m := range memories {
			chars += len(m.Content) + len(m.Context) + len(m.Tags)
		}
		estTokens := chars / 4 * 2 // two vectors per memory
		fmt.Printf("\nDry run — nothing changed.\n")
		fmt.Printf("Estimated input tokens: ~%d (~$%.4f at $0.004/M)\n", estTokens, float64(estTokens)*0.004/1e6)
		return nil
	}

	// Probe before touching anything. The index is dropped before re-embedding,
	// so a provider that is down (or a dimension that does not match the model)
	// would otherwise leave an empty index and exit "successfully".
	if err := probeEmbedder(embedClient, cfg.Embed.Dimension); err != nil {
		return fmt.Errorf("%w — nothing was changed", err)
	}

	// Rebuild only when the on-disk tables are the wrong width, or when asked to.
	// A re-run after an interruption finds the tables already right and must not
	// drop the rows the first run finished.
	needRebuild := force
	if existing, err := store.VecIndexDimensions(); err == nil {
		for _, dim := range existing {
			if dim != 0 && dim != cfg.Embed.Dimension {
				needRebuild = true
			}
		}
	}
	if needRebuild {
		if err := store.RebuildVecIndex(); err != nil {
			return fmt.Errorf("rebuilding vector index: %w", err)
		}
		fmt.Printf("Vector index rebuilt at %d dimensions.\n\n", cfg.Embed.Dimension)
	} else {
		fmt.Printf("Vector index already at %d dimensions; keeping it and skipping finished rows.\n\n", cfg.Embed.Dimension)
	}

	// Progress goes to stderr so a caller can pipe the summary on stdout.
	next := 0
	stats, err := store.ReembedAll(embedClient, memory.ReembedOptions{
		SkipCurrent: !force,
		Progress: func(string) {
			next++
			if next%25 == 0 {
				fmt.Fprintf(os.Stderr, "  ... %d re-embedded\n", next)
			}
		},
	})
	if err != nil {
		return fmt.Errorf("re-embedding: %w", err)
	}

	fmt.Printf("\nDone.\n")
	fmt.Printf("  memories re-embedded: %d/%d\n", stats.Memories, stats.TotalMemories)
	fmt.Printf("  moods re-embedded:    %d/%d\n", stats.Moods, stats.TotalMoods)
	if stats.Skipped > 0 {
		fmt.Printf("  left alone:           %d (already current, or nothing to embed)\n", stats.Skipped)
	}
	if stats.Failed > 0 {
		// A non-nil error becomes a non-zero exit status, which is how a script
		// or deploy step learns the run was incomplete.
		return fmt.Errorf("%d rows failed to embed; re-run `her reembed` to retry just those", stats.Failed)
	}
	return nil
}

// probeEmbedder makes one real embedding call and checks the width of the
// answer. It is the guard that lets `her reembed` fail before it drops anything:
// it catches a wrong key, a provider outage, a wrong base URL, and an
// embed.dimension that disagrees with the model's actual output.
func probeEmbedder(e memory.Embedder, dimension int) error {
	vec, err := e.Embed("dimension probe")
	if err != nil {
		return fmt.Errorf("embedding provider check failed: %w", err)
	}
	if len(vec) != dimension {
		return fmt.Errorf("the embedding model returned %d dimensions but embed.dimension is %d; fix embed.dimension in config.yaml", len(vec), dimension)
	}
	return nil
}

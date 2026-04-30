package cmd

import (
	"context"
	"fmt"
	"time"

	"her/config"
	"her/d1"
	"her/memory"

	"github.com/spf13/cobra"
)

// ---------------------------------------------------------------------------
// her sync — D1 synchronization commands
// ---------------------------------------------------------------------------

var syncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Manage D1 data synchronization",
	Long: `Push local data to D1 or pull remote data to local SQLite.

  her sync push — upload all local data to D1 (initial seeding)
  her sync pull — pull latest data from D1 into local her.db

Both operations are idempotent (safe to run multiple times).`,
}

var syncPushCmd = &cobra.Command{
	Use:   "push",
	Short: "Upload all local data to Cloudflare D1",
	Long: `Reads every synced table from local her.db and pushes all rows to D1
using INSERT OR REPLACE. This is the initial seeding operation for a
fresh D1 database. Safe to run multiple times.

Tables pushed: messages, summaries, memories, memory_links, reflections,
persona_versions, traits, persona_state, mood_entries.

Embedding columns are excluded — each machine generates its own embeddings.`,
	RunE: runSyncPush,
}

var syncPullCmd = &cobra.Command{
	Use:   "pull",
	Short: "Pull latest data from D1 into local SQLite",
	Long: `Fetches new rows from D1 and upserts them into local her.db. Same
operation that runs automatically on startup — this is for on-demand
sync without restarting the bot.

After pulling, run "her run" to backfill embeddings for new memories.`,
	RunE: runSyncPull,
}

func init() {
	rootCmd.AddCommand(syncCmd)
	syncCmd.AddCommand(syncPushCmd)
	syncCmd.AddCommand(syncPullCmd)
}

// runSyncPush handles `her sync push`.
func runSyncPush(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	synced, cleanup, err := makeSyncedStore(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	pushTimeout := time.Duration(cfg.Cloudflare.Sync.PushTimeout) * time.Second
	if pushTimeout == 0 {
		pushTimeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), pushTimeout)
	defer cancel()

	return synced.PushAll(ctx)
}

// runSyncPull handles `her sync pull`.
func runSyncPull(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	synced, cleanup, err := makeSyncedStore(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	pullTimeout := time.Duration(cfg.Cloudflare.Sync.PullTimeout) * time.Second
	if pullTimeout == 0 {
		pullTimeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), pullTimeout)
	defer cancel()

	if err := synced.Pull(ctx); err != nil {
		return err
	}

	// Embeddings aren't synced via D1 — each machine computes its own.
	// The next `her run` will backfill embeddings for any newly pulled
	// memories via the existing startup backfill goroutine.
	log.Info("run `her run` to backfill embeddings for newly pulled memories")
	return nil
}

// makeSyncedStore creates a SyncedStore for use by sync commands.
// Returns the store and a cleanup function. The cleanup function
// closes everything (carrier goroutine + SQLite connection).
func makeSyncedStore(cfg *config.Config) (*memory.SyncedStore, func(), error) {
	if cfg.Cloudflare.D1DatabaseID == "" {
		return nil, nil, fmt.Errorf("d1_database_id not set in config — D1 sync is disabled")
	}

	store, err := memory.NewStore(cfg.Memory.DBPath, cfg.Embed.Dimension)
	if err != nil {
		return nil, nil, fmt.Errorf("opening database: %w", err)
	}

	d1Client := d1.NewClient(cfg.Cloudflare.AccountID, cfg.Cloudflare.D1DatabaseID, cfg.Cloudflare.APIToken)
	if d1Client == nil {
		store.Close()
		return nil, nil, fmt.Errorf("failed to create D1 client — check cloudflare config")
	}

	synced, err := memory.NewSyncedStore(store, d1Client)
	if err != nil {
		store.Close()
		return nil, nil, fmt.Errorf("creating synced store: %w", err)
	}

	// Apply sync tuning from config (zero values keep defaults).
	if cfg.Cloudflare.Sync.BatchSize > 0 {
		synced.BatchSize = cfg.Cloudflare.Sync.BatchSize
	}
	if cfg.Cloudflare.Sync.PullPageSize > 0 {
		synced.PullPageSize = cfg.Cloudflare.Sync.PullPageSize
	}

	return synced, func() { synced.Close() }, nil
}

// Package memory provides SQLite-backed storage for conversations, facts,
// PII vault entries, and metrics. Everything lives in one database file.
package memory

import (
	"database/sql"
	"fmt"

	// The underscore import is a Go idiom: it imports the package purely for
	// its side effects (registering the SQLite driver with database/sql).
	// The package's init() function runs at startup and calls sql.Register().
	// You'll never call go-sqlite3 functions directly — you talk to it
	// through Go's standard database/sql interface.
	_ "github.com/mattn/go-sqlite3"

	// sqlite-vec adds vector search to SQLite via a virtual table module.
	// Auto() registers it as an auto-extension so every new connection gets it.
	// The cgo sub-package works with mattn/go-sqlite3 (our SQLite driver).
	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"

	// golang-migrate provides forward-only database migrations.
	// We use embedded migrations (compiled into the binary via //go:embed)
	// so tests work regardless of working directory. The iofs source reads
	// from an embed.FS instead of the filesystem.
	"her/migrations"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/sqlite3"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// Store wraps a SQLite database connection and provides methods for
// reading/writing messages, memories, metrics, and PII vault entries.
// In Go, this is how you build something like a Python class — a struct
// with methods attached to it.
type Store struct {
	db             *sql.DB
	dbPath         string // path to database file (needed for migrations)
	EmbedDimension int    // vector dimension for the vec_memories table (e.g. 768)

	// Zettelkasten memory linking — auto-connect new memories to similar existing ones.
	// When a memory is saved, we KNN-search for neighbors and create bidirectional
	// links. During retrieval, 1-hop traversal pulls in related memories that didn't
	// directly match the query. Think of it as Python's networkx graph, but
	// stored in SQLite so it persists across restarts.
	AutoLinkCount     int     // max links per new memory (0 = disabled)
	AutoLinkThreshold float64 // min cosine similarity to create a link (0.0-1.0)
}

// NewStore opens (or creates) the SQLite database at the given path
// and initializes all tables. The database file is created automatically
// by SQLite if it doesn't exist — no setup step needed.
//
// embedDimension is the vector size for the sqlite-vec index (e.g. 768
// for nomic-embed-text-v1.5). Pass 0 to skip creating the vector table
// (useful if embeddings aren't configured).
//
// NOTE: The initial CREATE TABLE statements still create the legacy "facts",
// "fact_links", and "vec_facts" tables for backward compatibility with older
// databases. The migration block at the end creates the renamed "memories",
// "memory_links", and "vec_memories" tables and copies data over. All runtime
// queries use the new table names.
func NewStore(dbPath string, embedDimension int) (*Store, error) {
	// Register sqlite-vec as an auto-extension BEFORE opening any connections.
	// This uses sqlite3_auto_extension() under the hood — every new connection
	// automatically loads the vec0 virtual table module. Think of it like
	// Python's sqlite3.enable_load_extension(), but baked into the driver.
	sqlite_vec.Auto()

	// sql.Open doesn't actually connect — it just validates the driver name
	// and prepares the connection. The real connection happens on first query.
	// The "?_journal_mode=WAL" enables Write-Ahead Logging, which allows
	// concurrent reads while writing. Much better performance for our use case.
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	// Ping actually tests the connection. This is where we'd catch issues
	// like bad file permissions or a corrupt database.
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("connecting to database: %w", err)
	}

	store := &Store{
		db:             db,
		dbPath:         dbPath,
		EmbedDimension: embedDimension,
	}

	// Run migrations to create/update schema
	if err := store.initTables(); err != nil {
		return nil, fmt.Errorf("running migrations: %w", err)
	}

	return store, nil
}

// Close cleanly shuts down the database connection.
// Always call this when the app exits (usually via defer in main.go).
func (s *Store) Close() error {
	return s.db.Close()
}

// DB returns the underlying database connection. This is exposed for
// advanced use cases like the sim command that need direct SQL access.
// Most code should use the Store methods instead of raw SQL.
func (s *Store) DB() *sql.DB {
	return s.db
}

// initTables runs database migrations from the embedded SQL files.
// Migrations are numbered sequentially (000001, 000002, etc.) and tracked
// in a schema_migrations table. This is forward-only — no rollbacks.
// Think of it as the Go equivalent of Wrangler's D1 migration system.
//
// The migrations are compiled into the binary via //go:embed (see
// migrations/embed.go), so they work regardless of working directory.
// This is what makes `go test ./memory` work — without embedding, the
// relative "file://migrations" path would fail from the package directory.
func (s *Store) initTables() error {
	// Create an iofs source from the embedded migration files. This
	// reads from the compiled-in embed.FS rather than the filesystem,
	// so no path resolution issues. The "." means "root of the FS" since
	// the embed directive already scoped to the migrations directory.
	source, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("creating migration source: %w", err)
	}

	m, err := migrate.NewWithSourceInstance(
		"iofs", source,
		"sqlite3://"+s.dbPath+"?_journal_mode=WAL&_foreign_keys=on",
	)
	if err != nil {
		return fmt.Errorf("creating migrator: %w", err)
	}
	defer m.Close()

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("applying migrations: %w", err)
	}

	// Create vector tables (need dynamic dimension, can't be in static SQL files)
	if s.EmbedDimension > 0 {
		vecDDL := fmt.Sprintf(
			`CREATE VIRTUAL TABLE IF NOT EXISTS vec_memories USING vec0(embedding float[%d] distance_metric=cosine)`,
			s.EmbedDimension,
		)
		if _, err := s.db.Exec(vecDDL); err != nil {
			return fmt.Errorf("creating vec_memories virtual table: %w", err)
		}

		moodVecDDL := fmt.Sprintf(
			`CREATE VIRTUAL TABLE IF NOT EXISTS vec_moods USING vec0(embedding float[%d] distance_metric=cosine)`,
			s.EmbedDimension,
		)
		if _, err := s.db.Exec(moodVecDDL); err != nil {
			return fmt.Errorf("creating vec_moods virtual table: %w", err)
		}
	}

	if err := s.initInboxTable(); err != nil {
		return err
	}

	return nil
}

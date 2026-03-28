package loader

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	_ "github.com/mattn/go-sqlite3" // SQLite driver (side-effect import)
)

// SidecarDB is a per-skill SQLite database that records execution history.
//
// Each skill gets its own <skill_name>.db file in its directory, containing
// every run's input args, output, timing, and an embedding vector for
// semantic search. This lets the agent check "did I already search for
// this?" before re-running a skill.
//
// The harness manages all writes — skills themselves never touch these
// databases. Same invisible-pipeline pattern as TTS.
//
// In Python terms, this is like having a SQLAlchemy session per plugin
// that auto-logs every call. The difference is we also store embeddings
// for semantic search via sqlite-vec.
type SidecarDB struct {
	db       *sql.DB
	embedDim int
}

// HistoryResult is a single past execution returned by SearchHistory.
// The agent sees these when it calls search_history to check cached results.
type HistoryResult struct {
	ID        int64
	Args      string        // JSON input args
	Result    string        // JSON output (truncated for agent context)
	ExitCode  int           // 0 = success, 1 = error
	Duration  time.Duration // wall-clock execution time
	Timestamp time.Time     // when the skill ran
	Age       string        // human-readable: "2h ago", "3d ago"
	Distance  float64       // cosine distance from query (0 = identical)
}

// OpenSidecar opens (or creates) a skill's sidecar database.
//
// The DB file lives at <skill.Dir>/<skill.Name>.db — right next to the
// skill's source code and skill.md. This makes skills fully portable:
// copy the directory, get everything including execution history.
//
// Returns an error if the skill is 4th-party (no sidecar access for
// unvetted skills — they haven't been reviewed and could be malicious).
//
// embedDim is the vector dimension for the KNN index (e.g., 768 for
// nomic-embed-text-v1.5). Pass 0 to skip creating the vector table.
func OpenSidecar(skill *Skill, embedDim int) (*SidecarDB, error) {
	if skill.TrustLevel == TrustFourthParty {
		return nil, fmt.Errorf("sidecar access denied for 4th-party skill %q", skill.Name)
	}

	// Register sqlite-vec extension. This is idempotent — safe to call
	// even though memory.NewStore() already called it. Under the hood
	// it uses sqlite3_auto_extension which deduplicates.
	sqlite_vec.Auto()

	dbPath := filepath.Join(skill.Dir, skill.Name+".db")
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("opening sidecar db: %w", err)
	}

	// Verify the connection is alive.
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("pinging sidecar db: %w", err)
	}

	// Create tables if they don't exist. CREATE IF NOT EXISTS is
	// idempotent — safe to run every time we open.
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS runs (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		args        TEXT NOT NULL,
		result      TEXT NOT NULL,
		exit_code   INTEGER NOT NULL,
		duration_ms INTEGER NOT NULL,
		timestamp   DATETIME DEFAULT CURRENT_TIMESTAMP
	)`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("creating runs table: %w", err)
	}

	// sqlite-vec virtual table for KNN search over execution results.
	// vec0 is the virtual table module — same as vec_facts in her.db.
	if embedDim > 0 {
		_, err = db.Exec(fmt.Sprintf(
			`CREATE VIRTUAL TABLE IF NOT EXISTS vec_runs USING vec0(
				embedding float[%d] distance_metric=cosine
			)`, embedDim))
		if err != nil {
			db.Close()
			return nil, fmt.Errorf("creating vec_runs table: %w", err)
		}
	}

	return &SidecarDB{db: db, embedDim: embedDim}, nil
}

// RecordRun saves a skill execution to the sidecar database.
//
// This is called by the runner after each successful 2nd-party skill
// execution. The embedding vector is a semantic representation of the
// args + result, used for later KNN search.
//
// exit_code is derived: 0 if result.Error is empty, 1 otherwise.
func (s *SidecarDB) RecordRun(args map[string]any, result *RunResult, embedding []float32) error {
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("marshaling args: %w", err)
	}

	// Use the JSON output if available, otherwise the raw output or error.
	resultText := ""
	if result.Output != nil {
		resultText = string(result.Output)
	} else if result.RawOut != "" {
		resultText = result.RawOut
	} else if result.Error != "" {
		resultText = result.Error
	}

	exitCode := 0
	if result.Error != "" {
		exitCode = 1
	}

	durationMs := result.Duration.Milliseconds()

	// Insert the run record.
	res, err := s.db.Exec(
		`INSERT INTO runs (args, result, exit_code, duration_ms) VALUES (?, ?, ?, ?)`,
		string(argsJSON), resultText, exitCode, durationMs,
	)
	if err != nil {
		return fmt.Errorf("inserting run: %w", err)
	}

	// Insert the embedding for KNN search, linked by rowid.
	if s.embedDim > 0 && len(embedding) > 0 {
		runID, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("getting run ID: %w", err)
		}

		vecBytes, err := sqlite_vec.SerializeFloat32(embedding)
		if err != nil {
			return fmt.Errorf("serializing embedding: %w", err)
		}

		_, err = s.db.Exec(
			`INSERT INTO vec_runs(rowid, embedding) VALUES (?, ?)`,
			runID, vecBytes,
		)
		if err != nil {
			return fmt.Errorf("inserting embedding: %w", err)
		}
	}

	return nil
}

// SearchHistory performs KNN semantic search over the skill's execution
// history. Returns the topK most relevant past runs, ordered by cosine
// similarity to the query vector.
//
// This is the same search pattern used by SemanticSearch in memory/store.go
// — embed a query, MATCH against the vec0 virtual table, JOIN back to the
// data table for metadata.
func (s *SidecarDB) SearchHistory(queryVec []float32, topK int) ([]HistoryResult, error) {
	if s.embedDim == 0 {
		return nil, fmt.Errorf("search not available: no embedding dimension configured")
	}

	queryBytes, err := sqlite_vec.SerializeFloat32(queryVec)
	if err != nil {
		return nil, fmt.Errorf("serializing query vector: %w", err)
	}

	rows, err := s.db.Query(
		`SELECT r.id, r.args, r.result, r.exit_code, r.duration_ms,
		        r.timestamp, v.distance
		 FROM vec_runs v
		 JOIN runs r ON r.id = v.rowid
		 WHERE v.embedding MATCH ?
		   AND k = ?
		 ORDER BY v.distance ASC`,
		queryBytes, topK,
	)
	if err != nil {
		return nil, fmt.Errorf("search query: %w", err)
	}
	defer rows.Close()

	var results []HistoryResult
	now := time.Now()

	for rows.Next() {
		var h HistoryResult
		var ts string
		var durationMs int64

		if err := rows.Scan(&h.ID, &h.Args, &h.Result, &h.ExitCode,
			&durationMs, &ts, &h.Distance); err != nil {
			return nil, fmt.Errorf("scanning result: %w", err)
		}

		h.Duration = time.Duration(durationMs) * time.Millisecond
		h.Timestamp, _ = time.Parse("2006-01-02 15:04:05", ts)
		h.Age = formatAge(now, h.Timestamp)

		// Truncate result for agent context — full results can be huge.
		if len(h.Result) > 500 {
			h.Result = h.Result[:500] + "..."
		}

		results = append(results, h)
	}

	return results, nil
}

// Close closes the sidecar database connection.
func (s *SidecarDB) Close() error {
	return s.db.Close()
}

// formatAge returns a human-readable age string like "2h ago" or "3d ago".
func formatAge(now, then time.Time) string {
	d := now.Sub(then)

	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1d ago"
		}
		return fmt.Sprintf("%dd ago", days)
	}
}

package memory

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ─── Mood Entries (v2 — Apple State-of-Mind style) ─────────────────
//
// Replaces the old 1-5 rating schema (now in _junkdrawer/store_mood.go).
// See docs/plans/PLAN-mood-tracking-redesign.md for the design. The
// table has two kinds: "momentary" for a single captured moment,
// "daily" for an end-of-day rollup. Source tracks provenance:
// "inferred" (mood agent), "confirmed" (user tapped a proposal), or
// "manual" (user ran /mood).

// MoodKind is the row's kind discriminator — controls which queries
// pick it up (e.g. daily rollup only scans momentary entries from
// today).
type MoodKind string

const (
	MoodKindMomentary MoodKind = "momentary"
	MoodKindDaily     MoodKind = "daily"
)

// MoodSource names where an entry came from. Inferred entries are
// treated with skepticism by downstream consumers (graphs weight them
// less, prompt layer labels them "maybe").
type MoodSource string

const (
	MoodSourceInferred  MoodSource = "inferred"
	MoodSourceConfirmed MoodSource = "confirmed"
	MoodSourceManual    MoodSource = "manual"
)

// MoodEntry mirrors one row in the mood_entries table. Labels and
// Associations are JSON arrays on disk but live-decoded to []string
// for ergonomic use in Go.
type MoodEntry struct {
	ID             int64
	Timestamp      time.Time
	Kind           MoodKind
	Valence        int // 1-7
	Labels         []string
	Associations   []string
	Note           string
	Source         MoodSource
	Confidence     float64
	ConversationID string // empty means NULL (manual entries typically have no conv)

	// Embedding is the cached note+labels vector. Nil when the entry
	// was saved without an embedding (e.g. manual entries in tests
	// where we run without sqlite-vec). Dedup queries skip entries
	// whose embedding is nil.
	Embedding []float32

	// Distance is populated by SimilarMoodEntriesWithin — cosine
	// distance from the query vector. 0 on other read paths.
	Distance float64

	CreatedAt time.Time
}

// SaveMoodEntry inserts a new mood row and mirrors its embedding into
// the vec_moods virtual table for KNN lookups. The returned ID is the
// new row's primary key. Embedding may be nil (tests, environments
// without sqlite-vec); when present, its length must match the
// store's EmbedDimension.
//
// Labels and Associations are stored as JSON arrays of the given
// strings. Validation (against the mood vocab) happens in the agent
// layer before calling here — the store layer is intentionally dumb
// about vocabulary.
func (s *Store) SaveMoodEntry(entry *MoodEntry) (int64, error) {
	if entry == nil {
		return 0, fmt.Errorf("SaveMoodEntry: nil entry")
	}
	if entry.Valence < 1 || entry.Valence > 7 {
		return 0, fmt.Errorf("SaveMoodEntry: valence %d out of range [1,7]", entry.Valence)
	}
	if entry.Kind == "" {
		entry.Kind = MoodKindMomentary
	}
	if entry.Source == "" {
		entry.Source = MoodSourceInferred
	}
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now()
	}

	labels, err := marshalStringArray(entry.Labels)
	if err != nil {
		return 0, fmt.Errorf("SaveMoodEntry: labels: %w", err)
	}
	associations, err := marshalStringArray(entry.Associations)
	if err != nil {
		return 0, fmt.Errorf("SaveMoodEntry: associations: %w", err)
	}

	// Serialize embedding to the BLOB column (source of truth). We
	// also mirror it into vec_moods below for KNN.
	var embBlob any
	if len(entry.Embedding) > 0 {
		b, err := serializeEmbedding(entry.Embedding)
		if err != nil {
			return 0, fmt.Errorf("SaveMoodEntry: embedding: %w", err)
		}
		embBlob = b
	}

	var convID any = entry.ConversationID
	if entry.ConversationID == "" {
		convID = nil
	}

	res, err := s.db.Exec(
		`INSERT INTO mood_entries
		   (ts, kind, valence, labels, associations, note, source,
		    confidence, conversation_id, embedding)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		entry.Timestamp.UTC(),
		string(entry.Kind),
		entry.Valence,
		labels,
		associations,
		entry.Note,
		string(entry.Source),
		entry.Confidence,
		convID,
		embBlob,
	)
	if err != nil {
		return 0, fmt.Errorf("SaveMoodEntry: insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("SaveMoodEntry: last insert id: %w", err)
	}

	// Mirror into vec_moods for KNN. If the virtual table doesn't
	// exist (EmbedDimension=0 at store init), skip silently — tests
	// and minimal setups still get a working mood store; they just
	// can't dedup by semantics.
	if len(entry.Embedding) > 0 && s.EmbedDimension > 0 {
		vecBytes, err := serializeEmbedding(entry.Embedding)
		if err != nil {
			return id, fmt.Errorf("SaveMoodEntry: serialize for vec_moods: %w", err)
		}
		if _, err := s.db.Exec(
			`INSERT INTO vec_moods(rowid, embedding) VALUES (?, ?)`,
			id, vecBytes,
		); err != nil {
			// Log-style warning (match store_facts.go pattern). The
			// row is already saved — dedup just won't find it.
			fmt.Printf("[mood] warning: vec_moods insert failed for entry %d: %v\n", id, err)
		}
	}

	return id, nil
}

// UpdateMoodEntry refines an existing mood entry in place — updating
// labels, associations, note, confidence, and embedding while
// preserving the original timestamp and ID. This is the "same mood,
// more detail" path: the user has been processing the same emotional
// state for a while and the newer inference has richer context.
//
// The updated_at column is stamped so consumers can distinguish
// original entries from refined ones. The vec_moods virtual table
// embedding is also replaced so future KNN dedup sees the latest
// vector geometry.
func (s *Store) UpdateMoodEntry(id int64, entry *MoodEntry) error {
	if entry == nil {
		return fmt.Errorf("UpdateMoodEntry: nil entry")
	}

	labels, err := marshalStringArray(entry.Labels)
	if err != nil {
		return fmt.Errorf("UpdateMoodEntry: labels: %w", err)
	}
	associations, err := marshalStringArray(entry.Associations)
	if err != nil {
		return fmt.Errorf("UpdateMoodEntry: associations: %w", err)
	}

	var embBlob any
	if len(entry.Embedding) > 0 {
		b, err := serializeEmbedding(entry.Embedding)
		if err != nil {
			return fmt.Errorf("UpdateMoodEntry: embedding: %w", err)
		}
		embBlob = b
	}

	_, err = s.db.Exec(
		`UPDATE mood_entries
		 SET valence = ?, labels = ?, associations = ?, note = ?,
		     confidence = ?, embedding = ?, updated_at = CURRENT_TIMESTAMP
		 WHERE id = ?`,
		entry.Valence, labels, associations, entry.Note,
		entry.Confidence, embBlob, id,
	)
	if err != nil {
		return fmt.Errorf("UpdateMoodEntry: update: %w", err)
	}

	// Replace the vec_moods embedding so KNN dedup uses the latest vector.
	if len(entry.Embedding) > 0 && s.EmbedDimension > 0 {
		vecBytes, err := serializeEmbedding(entry.Embedding)
		if err != nil {
			return fmt.Errorf("UpdateMoodEntry: serialize for vec_moods: %w", err)
		}
		// sqlite-vec uses DELETE + INSERT to update a row.
		s.db.Exec(`DELETE FROM vec_moods WHERE rowid = ?`, id)
		if _, err := s.db.Exec(
			`INSERT INTO vec_moods(rowid, embedding) VALUES (?, ?)`,
			id, vecBytes,
		); err != nil {
			fmt.Printf("[mood] warning: vec_moods update failed for entry %d: %v\n", id, err)
		}
	}

	return nil
}

// LatestMoodEntry returns the most recent mood entry of the given
// kind, or nil when none exist. Empty kind means any.
func (s *Store) LatestMoodEntry(kind MoodKind) (*MoodEntry, error) {
	q := `SELECT id, ts, kind, valence, labels, associations, note, source,
	             confidence, conversation_id, embedding, created_at
	      FROM mood_entries`
	args := []any{}
	if kind != "" {
		q += ` WHERE kind = ?`
		args = append(args, string(kind))
	}
	q += ` ORDER BY ts DESC LIMIT 1`

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("LatestMoodEntry: %w", err)
	}
	defer rows.Close()

	entries, err := scanMoodEntries(rows)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	return &entries[0], nil
}

// RecentMoodEntries returns the most recent N entries of the given
// kind, newest first. Used by the prompt layer and the daily rollup.
func (s *Store) RecentMoodEntries(kind MoodKind, limit int) ([]MoodEntry, error) {
	if limit <= 0 {
		limit = 10
	}
	q := `SELECT id, ts, kind, valence, labels, associations, note, source,
	             confidence, conversation_id, embedding, created_at
	      FROM mood_entries`
	args := []any{}
	if kind != "" {
		q += ` WHERE kind = ?`
		args = append(args, string(kind))
	}
	q += ` ORDER BY ts DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("RecentMoodEntries: %w", err)
	}
	defer rows.Close()
	return scanMoodEntries(rows)
}

// MoodEntriesInRange returns every entry of the given kind whose
// timestamp falls within [from, to]. Kind may be empty. Ordered
// oldest-first, which is what charts want.
func (s *Store) MoodEntriesInRange(kind MoodKind, from, to time.Time) ([]MoodEntry, error) {
	q := `SELECT id, ts, kind, valence, labels, associations, note, source,
	             confidence, conversation_id, embedding, created_at
	      FROM mood_entries
	      WHERE ts >= ? AND ts <= ?`
	args := []any{from.UTC(), to.UTC()}
	if kind != "" {
		q += ` AND kind = ?`
		args = append(args, string(kind))
	}
	q += ` ORDER BY ts ASC`

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("MoodEntriesInRange: %w", err)
	}
	defer rows.Close()
	return scanMoodEntries(rows)
}

// SimilarMoodEntriesWithin finds existing mood entries whose embedding
// is close to the query vector (cosine distance) AND whose timestamp
// is within (now - window, now]. The mood agent uses this to decide
// whether a new inferred mood is a duplicate of one logged within the
// dedup window — see docs/plans/PLAN-mood-tracking-redesign.md.
//
// The `now` parameter is injected so the caller controls the window
// anchor; the sim passes a frozen FakeClock value, the live bot
// passes time.Now().
//
// Returns a slice sorted by distance (closest first). `limit` caps the
// number of results. Returns nil if the store was initialized without
// a vec_moods virtual table (EmbedDimension=0).
func (s *Store) SimilarMoodEntriesWithin(now time.Time, embedding []float32, window time.Duration, limit int) ([]MoodEntry, error) {
	if s.EmbedDimension == 0 || len(embedding) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 5
	}

	queryBytes, err := serializeEmbedding(embedding)
	if err != nil {
		return nil, fmt.Errorf("SimilarMoodEntriesWithin: serialize: %w", err)
	}

	// We request a few extra rows from vec_moods because the time-
	// window filter happens on the SQL side; vec0's KNN doesn't know
	// about our time predicate. Ordering is by distance inside the JOIN.
	// k must be a literal in the vec0 WHERE, hence the Go-side limit.
	cutoff := now.Add(-window).UTC()

	rows, err := s.db.Query(`
		SELECT m.id, m.ts, m.kind, m.valence, m.labels, m.associations, m.note,
		       m.source, m.confidence, m.conversation_id, m.embedding, m.created_at,
		       v.distance
		FROM vec_moods v
		JOIN mood_entries m ON m.id = v.rowid
		WHERE v.embedding MATCH ?
		  AND k = ?
		  AND m.ts >= ?
		ORDER BY v.distance ASC`,
		queryBytes, limit*4, cutoff,
	)
	if err != nil {
		return nil, fmt.Errorf("SimilarMoodEntriesWithin: query: %w", err)
	}
	defer rows.Close()

	var out []MoodEntry
	for rows.Next() {
		var (
			e            MoodEntry
			labelsJSON   string
			assocJSON    string
			kindStr      string
			sourceStr    string
			convNullable sql.NullString
			embData      []byte
			distance     float64
		)
		if err := rows.Scan(
			&e.ID, &e.Timestamp, &kindStr, &e.Valence, &labelsJSON, &assocJSON,
			&e.Note, &sourceStr, &e.Confidence, &convNullable, &embData, &e.CreatedAt,
			&distance,
		); err != nil {
			return nil, fmt.Errorf("SimilarMoodEntriesWithin: scan: %w", err)
		}
		e.Kind = MoodKind(kindStr)
		e.Source = MoodSource(sourceStr)
		if convNullable.Valid {
			e.ConversationID = convNullable.String
		}
		e.Labels = unmarshalStringArray(labelsJSON)
		e.Associations = unmarshalStringArray(assocJSON)
		e.Embedding = deserializeEmbedding(embData)
		e.Distance = distance

		out = append(out, e)
		if len(out) >= limit {
			break
		}
	}
	return out, rows.Err()
}

// DeleteMoodEntry removes a row and its vec_moods mirror. Used by the
// /mood wizard's "cancel before final step" path and by tests.
func (s *Store) DeleteMoodEntry(id int64) error {
	if _, err := s.db.Exec(`DELETE FROM mood_entries WHERE id = ?`, id); err != nil {
		return fmt.Errorf("DeleteMoodEntry: %w", err)
	}
	// vec_moods delete is best-effort — if the table doesn't exist,
	// ignore the error (same pattern as vec_memories inserts).
	_, _ = s.db.Exec(`DELETE FROM vec_moods WHERE rowid = ?`, id)
	return nil
}

// ─── Pending Mood Proposals ────────────────────────────────────────
//
// Medium-confidence inferences the bot sent to Telegram as an inline-
// keyboard proposal. Rows track lifecycle: pending → confirmed |
// rejected | expired.

// MoodProposalStatus names lifecycle states.
type MoodProposalStatus string

const (
	MoodProposalPending   MoodProposalStatus = "pending"
	MoodProposalConfirmed MoodProposalStatus = "confirmed"
	MoodProposalRejected  MoodProposalStatus = "rejected"
	MoodProposalExpired   MoodProposalStatus = "expired"
)

// PendingMoodProposal mirrors one row in pending_mood_proposals.
// ProposalJSON is the serialized MoodEntry fields the agent would
// save if the user confirms.
type PendingMoodProposal struct {
	ID                int64
	Timestamp         time.Time
	TelegramChatID    int64
	TelegramMessageID int64
	ProposalJSON      json.RawMessage
	Status            MoodProposalStatus
	ExpiresAt         time.Time
}

// SavePendingMoodProposal inserts a proposal row tied to the Telegram
// message that carries the inline keyboard.
func (s *Store) SavePendingMoodProposal(p *PendingMoodProposal) (int64, error) {
	if p == nil {
		return 0, fmt.Errorf("SavePendingMoodProposal: nil")
	}
	if p.TelegramChatID == 0 || p.TelegramMessageID == 0 {
		return 0, fmt.Errorf("SavePendingMoodProposal: missing chat/message id")
	}
	if p.ExpiresAt.IsZero() {
		return 0, fmt.Errorf("SavePendingMoodProposal: missing ExpiresAt")
	}
	if p.Status == "" {
		p.Status = MoodProposalPending
	}
	if p.Timestamp.IsZero() {
		p.Timestamp = time.Now()
	}

	res, err := s.db.Exec(
		`INSERT INTO pending_mood_proposals
		   (ts, telegram_chat_id, telegram_message_id, proposal_json, status, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		p.Timestamp.UTC(),
		p.TelegramChatID,
		p.TelegramMessageID,
		string(p.ProposalJSON),
		string(p.Status),
		p.ExpiresAt.UTC(),
	)
	if err != nil {
		return 0, fmt.Errorf("SavePendingMoodProposal: %w", err)
	}
	return res.LastInsertId()
}

// PendingMoodProposalByMessageID looks up a proposal by its Telegram
// message ID. Returns (nil, nil) when no matching row exists. The
// callback handler uses this on tap.
func (s *Store) PendingMoodProposalByMessageID(chatID, msgID int64) (*PendingMoodProposal, error) {
	row := s.db.QueryRow(
		`SELECT id, ts, telegram_chat_id, telegram_message_id,
		        proposal_json, status, expires_at
		 FROM pending_mood_proposals
		 WHERE telegram_chat_id = ? AND telegram_message_id = ?
		 LIMIT 1`,
		chatID, msgID,
	)
	var (
		p       PendingMoodProposal
		payload string
		status  string
	)
	err := row.Scan(&p.ID, &p.Timestamp, &p.TelegramChatID, &p.TelegramMessageID,
		&payload, &status, &p.ExpiresAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("PendingMoodProposalByMessageID: %w", err)
	}
	p.ProposalJSON = json.RawMessage(payload)
	p.Status = MoodProposalStatus(status)
	return &p, nil
}

// DuePendingMoodProposals returns proposals whose expiry has passed
// but still have status=pending. The sweeper loop calls this on its
// tick and flips each to status=expired after editing the Telegram
// message.
func (s *Store) DuePendingMoodProposals(now time.Time) ([]PendingMoodProposal, error) {
	rows, err := s.db.Query(
		`SELECT id, ts, telegram_chat_id, telegram_message_id,
		        proposal_json, status, expires_at
		 FROM pending_mood_proposals
		 WHERE status = ? AND expires_at <= ?
		 ORDER BY expires_at ASC`,
		string(MoodProposalPending), now.UTC(),
	)
	if err != nil {
		return nil, fmt.Errorf("DuePendingMoodProposals: %w", err)
	}
	defer rows.Close()

	var out []PendingMoodProposal
	for rows.Next() {
		var (
			p       PendingMoodProposal
			payload string
			status  string
		)
		if err := rows.Scan(&p.ID, &p.Timestamp, &p.TelegramChatID, &p.TelegramMessageID,
			&payload, &status, &p.ExpiresAt); err != nil {
			return nil, fmt.Errorf("DuePendingMoodProposals: scan: %w", err)
		}
		p.ProposalJSON = json.RawMessage(payload)
		p.Status = MoodProposalStatus(status)
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpdatePendingMoodProposalStatus changes a proposal's status. The
// callback handler flips to confirmed/rejected; the sweeper flips to
// expired.
func (s *Store) UpdatePendingMoodProposalStatus(id int64, status MoodProposalStatus) error {
	res, err := s.db.Exec(
		`UPDATE pending_mood_proposals SET status = ? WHERE id = ?`,
		string(status), id,
	)
	if err != nil {
		return fmt.Errorf("UpdatePendingMoodProposalStatus: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("UpdatePendingMoodProposalStatus: no row with id %d", id)
	}
	return nil
}

// ─── Helpers ──────────────────────────────────────────────────────

// marshalStringArray serializes a []string as a JSON array. A nil or
// empty slice becomes `[]` (never `null`) so downstream consumers can
// assume a valid array in every row.
func marshalStringArray(s []string) (string, error) {
	if len(s) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// unmarshalStringArray decodes a JSON array of strings, tolerant to
// empty input and malformed rows. On decode failure returns nil
// rather than erroring; the row is still usable, it just won't show
// labels/associations.
func unmarshalStringArray(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" || s == "null" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

// scanMoodEntries reads rows from a SELECT that matches the order
// (id, ts, kind, valence, labels, associations, note, source,
// confidence, conversation_id, embedding, created_at). Distance is
// not populated — use SimilarMoodEntriesWithin for that.
func scanMoodEntries(rows *sql.Rows) ([]MoodEntry, error) {
	var out []MoodEntry
	for rows.Next() {
		var (
			e            MoodEntry
			labelsJSON   string
			assocJSON    string
			kindStr      string
			sourceStr    string
			convNullable sql.NullString
			embData      []byte
		)
		if err := rows.Scan(
			&e.ID, &e.Timestamp, &kindStr, &e.Valence, &labelsJSON, &assocJSON,
			&e.Note, &sourceStr, &e.Confidence, &convNullable, &embData, &e.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanMoodEntries: %w", err)
		}
		e.Kind = MoodKind(kindStr)
		e.Source = MoodSource(sourceStr)
		if convNullable.Valid {
			e.ConversationID = convNullable.String
		}
		e.Labels = unmarshalStringArray(labelsJSON)
		e.Associations = unmarshalStringArray(assocJSON)
		e.Embedding = deserializeEmbedding(embData)
		out = append(out, e)
	}
	return out, rows.Err()
}

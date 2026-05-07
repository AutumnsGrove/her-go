package memory

import (
	"encoding/json"
	"fmt"
	"time"
)

// DreamAudit records a single operation performed by the memory dreamer.
type DreamAudit struct {
	ID         int64
	Timestamp  time.Time
	Operation  string  // "merge", "expire", "promote", "split"
	SourceIDs  []int64 // memory IDs affected
	ResultID   int64   // new/updated memory ID (0 for expire)
	BeforeText string
	AfterText  string
	Reason     string
	DryRun     bool
}

// SaveDreamAudit logs a memory dreamer operation to the audit table.
func (s *SQLiteStore) SaveDreamAudit(op string, sourceIDs []int64, resultID int64, before, after, reason string, dryRun bool) error {
	idsJSON, err := json.Marshal(sourceIDs)
	if err != nil {
		return fmt.Errorf("marshalling source IDs: %w", err)
	}

	dryRunInt := 0
	if dryRun {
		dryRunInt = 1
	}

	_, err = s.db.Exec(
		`INSERT INTO dream_audit (operation, source_ids, result_id, before_text, after_text, reason, dry_run)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		op, string(idsJSON), resultID, before, after, reason, dryRunInt,
	)
	if err != nil {
		return fmt.Errorf("saving dream audit: %w", err)
	}
	return nil
}

// RecentDreamAudits returns the most recent N audit entries, newest first.
func (s *SQLiteStore) RecentDreamAudits(limit int) ([]DreamAudit, error) {
	rows, err := s.db.Query(
		`SELECT id, timestamp, operation, source_ids, result_id,
		        COALESCE(before_text, ''), COALESCE(after_text, ''),
		        COALESCE(reason, ''), dry_run
		 FROM dream_audit ORDER BY id DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("querying dream audits: %w", err)
	}
	defer rows.Close()

	var audits []DreamAudit
	for rows.Next() {
		var a DreamAudit
		var idsJSON string
		if err := rows.Scan(&a.ID, &a.Timestamp, &a.Operation, &idsJSON,
			&a.ResultID, &a.BeforeText, &a.AfterText, &a.Reason, &a.DryRun); err != nil {
			return nil, fmt.Errorf("scanning dream audit: %w", err)
		}
		if err := json.Unmarshal([]byte(idsJSON), &a.SourceIDs); err != nil {
			log.Warn("dream audit: corrupt source_ids JSON", "id", a.ID, "err", err)
		}
		audits = append(audits, a)
	}
	return audits, rows.Err()
}

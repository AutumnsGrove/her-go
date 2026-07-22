package memory

import (
	"fmt"
	"time"
)

// Agent role constants for the SaveMetric agentRole parameter.
// Defined once here so all callers reference these instead of bare strings.
const (
	RoleDriver        = "driver"
	RoleMemory        = "memory"
	RoleMood          = "mood"
	RoleIntrospection = "introspection"
	RoleChat          = "chat"
	RoleDream         = "dream"
	RoleCompaction    = "compaction"
	RoleVision        = "vision"
	RoleClassifier    = "classifier"
	// RolePolaris tags spend that happened inside Polaris (a separate
	// self-hosted service, not one of her-go's own LLM calls) — kept
	// distinct from RoleDriver/RoleChat/etc so cost dashboards can break
	// out "how much did delegating to Polaris cost" on its own.
	RolePolaris = "polaris"
)

// MetricInput bundles all data for a single LLM call metric. Replaces
// the old 12-parameter SaveMetric signature with a readable struct.
//
// In Python you'd use a dataclass for this. In Go, a plain struct with
// exported fields serves the same purpose — and since Go has zero values
// for every type, callers only need to set the fields they care about.
type MetricInput struct {
	Model            string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CostUSD          float64
	LatencyMs        int
	MessageID        int64
	IsFallback       bool
	AgentRole        string
	CacheReadTokens  int
	CacheWriteTokens int
	Provider         string
}

// Metric represents token usage and cost data for a single LLM call.
type Metric struct {
	ID               int64
	Timestamp        time.Time
	Model            string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CostUSD          float64
	LatencyMs        int
	MessageID        int64
}

// SaveMetric logs token usage and cost data for an LLM call.
func (s *SQLiteStore) SaveMetric(m MetricInput) error {
	var msgID interface{} = m.MessageID
	if m.MessageID == 0 {
		msgID = nil
	}
	_, err := s.db.Exec(
		`INSERT INTO metrics (model, prompt_tokens, completion_tokens, total_tokens, cost_usd, latency_ms, message_id, is_fallback, agent_role, cache_read_tokens, cache_write_tokens, provider)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.Model, m.PromptTokens, m.CompletionTokens, m.TotalTokens, m.CostUSD, m.LatencyMs, msgID, m.IsFallback, m.AgentRole, m.CacheReadTokens, m.CacheWriteTokens, m.Provider,
	)
	if err != nil {
		return fmt.Errorf("saving metric: %w", err)
	}
	return nil
}

// CostForMessage returns the total cost across ALL agent roles for a given
// trigger message ID. This is the authoritative cost for a turn — it includes
// driver, chat, memory, introspection, mood, classifier, vision, and compaction.
// Used by TurnEnd cost reporting instead of summing partial struct fields.
func (s *SQLiteStore) CostForMessage(messageID int64) (float64, error) {
	var cost float64
	err := s.db.QueryRow(
		`SELECT COALESCE(SUM(cost_usd), 0) FROM metrics WHERE message_id = ?`,
		messageID,
	).Scan(&cost)
	if err != nil {
		return 0, fmt.Errorf("querying cost for message: %w", err)
	}
	return cost, nil
}

// CostSince returns the total cost_usd for a given agent role since the
// given timestamp. Used by the dream cycle to sum costs across all steps
// without threading cost return values through every nested function.
func (s *SQLiteStore) CostSince(role string, since time.Time) (float64, error) {
	var cost float64
	err := s.db.QueryRow(
		`SELECT COALESCE(SUM(cost_usd), 0) FROM metrics
		 WHERE agent_role = ? AND timestamp >= ?`,
		role, since.Format("2006-01-02 15:04:05"),
	).Scan(&cost)
	if err != nil {
		return 0, fmt.Errorf("querying cost since: %w", err)
	}
	return cost, nil
}

// Stats holds aggregate usage statistics for the /stats command.
// CommandCount holds usage info for a single slash command.
type CommandCount struct {
	Command string
	Count   int
}

type Stats struct {
	TotalMessages    int
	UserMessages     int
	MiraMessages     int
	TotalFacts       int
	UserFacts        int
	SelfFacts        int
	TotalTokens      int
	TotalCostUSD     float64
	ChatTokens       int
	ChatCostUSD      float64
	AgentTokens      int
	AgentCostUSD     float64
	AvgLatencyMs     int
	ConversationDays int // how many distinct days have messages
	TotalCommands    int
	CommandCounts    []CommandCount // per-command breakdown, sorted by count desc
}

// GetStats computes aggregate usage statistics across all data.
// Uses several small queries rather than one giant join — clearer
// and fast enough for our scale.
func (s *SQLiteStore) GetStats() (*Stats, error) {
	st := &Stats{}

	// scanInt runs a single-column QueryRow and scans the result into *dest.
	// On error it logs a warning and leaves *dest at its zero value (0).
	// This keeps GetStats fault-tolerant — a missing table or schema change
	// won't blow up the whole /stats command.
	scanInt := func(query string, dest ...interface{}) {
		if err := s.db.QueryRow(query).Scan(dest...); err != nil {
			log.Warn("GetStats: scan failed", "query", query, "err", err)
		}
	}

	// Message counts by role.
	scanInt(`SELECT COUNT(*) FROM messages`, &st.TotalMessages)
	scanInt(`SELECT COUNT(*) FROM messages WHERE role = 'user'`, &st.UserMessages)
	scanInt(`SELECT COUNT(*) FROM messages WHERE role = 'assistant'`, &st.MiraMessages)

	// Fact counts by subject.
	scanInt(`SELECT COUNT(*) FROM facts WHERE active = 1`, &st.TotalFacts)
	scanInt(`SELECT COUNT(*) FROM facts WHERE active = 1 AND COALESCE(subject, 'user') = 'user'`, &st.UserFacts)
	scanInt(`SELECT COUNT(*) FROM facts WHERE active = 1 AND COALESCE(subject, 'user') = 'self'`, &st.SelfFacts)

	// Token + cost totals, split by chat vs agent model.
	// Chat models have latency_ms > 0 (agent calls log latency as 0).
	scanInt(`SELECT COALESCE(SUM(total_tokens), 0), COALESCE(SUM(cost_usd), 0) FROM metrics`, &st.TotalTokens, &st.TotalCostUSD)
	scanInt(`SELECT COALESCE(SUM(total_tokens), 0), COALESCE(SUM(cost_usd), 0) FROM metrics WHERE latency_ms > 0`, &st.ChatTokens, &st.ChatCostUSD)
	scanInt(`SELECT COALESCE(SUM(total_tokens), 0), COALESCE(SUM(cost_usd), 0) FROM metrics WHERE latency_ms = 0`, &st.AgentTokens, &st.AgentCostUSD)

	// Average chat latency (exclude agent calls which have 0 latency).
	scanInt(`SELECT CAST(COALESCE(AVG(latency_ms), 0) AS INTEGER) FROM metrics WHERE latency_ms > 0`, &st.AvgLatencyMs)

	// Distinct days with messages (gives a sense of how many days active).
	scanInt(`SELECT COUNT(DISTINCT DATE(timestamp)) FROM messages`, &st.ConversationDays)

	// Command usage from the command_log table.
	scanInt(`SELECT COUNT(*) FROM command_log`, &st.TotalCommands)
	rows, err := s.db.Query(
		`SELECT command, COUNT(*) as cnt FROM command_log
		 GROUP BY command ORDER BY cnt DESC`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var cc CommandCount
			if rows.Scan(&cc.Command, &cc.Count) == nil {
				st.CommandCounts = append(st.CommandCounts, cc)
			}
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("iterating rows: %w", err)
		}
	}

	return st, nil
}

// ModelUsage holds per-model cost and token totals for the usage command.
type ModelUsage struct {
	Model            string
	Calls            int
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CostUSD          float64
}

// PeriodUsage holds cost totals for a time period (today, 7d, 30d, all-time).
type PeriodUsage struct {
	Label   string
	Calls   int
	Tokens  int
	CostUSD float64
}

// RoleUsage holds per-agent-role cost breakdown for a time period.
type RoleUsage struct {
	Role    string
	Calls   int
	Tokens  int
	CostUSD float64
}

// UsageReport bundles everything the `her usage` command needs.
type UsageReport struct {
	Periods      []PeriodUsage
	ByModel      []ModelUsage
	ByRoleToday  []RoleUsage
	ByRole7Days  []RoleUsage
	ByRole30Days []RoleUsage
}

// GetUsageReport builds a complete cost/token breakdown.
// Queries the metrics table with different time windows and a per-model
// GROUP BY. Each query is small and fast — SQLite handles this easily
// at our scale.
func (s *SQLiteStore) GetUsageReport() (*UsageReport, error) {
	r := &UsageReport{}

	// Time-windowed totals: today, last 7 days, last 30 days, all-time.
	// DATE('now') gives today in UTC — same timezone SQLite uses for
	// DEFAULT CURRENT_TIMESTAMP, so the windows are consistent.
	periods := []struct {
		label string
		where string // SQL WHERE clause fragment
	}{
		{"Today", "timestamp >= DATE('now')"},
		{"Last 7 days", "timestamp >= DATE('now', '-7 days')"},
		{"Last 30 days", "timestamp >= DATE('now', '-30 days')"},
		{"All time", "1=1"},
	}

	for _, p := range periods {
		var pu PeriodUsage
		pu.Label = p.label
		err := s.db.QueryRow(
			fmt.Sprintf(`SELECT COUNT(*), COALESCE(SUM(total_tokens), 0), COALESCE(SUM(cost_usd), 0)
			 FROM metrics WHERE %s`, p.where),
		).Scan(&pu.Calls, &pu.Tokens, &pu.CostUSD)
		if err != nil {
			return nil, fmt.Errorf("querying period %s: %w", p.label, err)
		}
		r.Periods = append(r.Periods, pu)
	}

	// Per-role breakdown for each time window.
	roleWindows := []struct {
		where string
		dest  *[]RoleUsage
	}{
		{"timestamp >= DATE('now')", &r.ByRoleToday},
		{"timestamp >= DATE('now', '-7 days')", &r.ByRole7Days},
		{"timestamp >= DATE('now', '-30 days')", &r.ByRole30Days},
	}
	for _, rw := range roleWindows {
		rows, err := s.db.Query(
			fmt.Sprintf(`SELECT COALESCE(agent_role, 'unknown'), COUNT(*),
			        COALESCE(SUM(total_tokens), 0), COALESCE(SUM(cost_usd), 0)
			 FROM metrics WHERE %s
			 GROUP BY agent_role
			 ORDER BY SUM(cost_usd) DESC`, rw.where))
		if err != nil {
			return nil, fmt.Errorf("querying role usage: %w", err)
		}
		for rows.Next() {
			var ru RoleUsage
			if err := rows.Scan(&ru.Role, &ru.Calls, &ru.Tokens, &ru.CostUSD); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scanning role row: %w", err)
			}
			*rw.dest = append(*rw.dest, ru)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("iterating role rows: %w", err)
		}
		rows.Close()
	}

	// Per-model breakdown, sorted by total cost descending.
	rows, err := s.db.Query(
		`SELECT model, COUNT(*), COALESCE(SUM(prompt_tokens), 0),
		        COALESCE(SUM(completion_tokens), 0), COALESCE(SUM(total_tokens), 0),
		        COALESCE(SUM(cost_usd), 0)
		 FROM metrics
		 GROUP BY model
		 ORDER BY SUM(cost_usd) DESC`)
	if err != nil {
		return nil, fmt.Errorf("querying model usage: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var m ModelUsage
		if err := rows.Scan(&m.Model, &m.Calls, &m.PromptTokens, &m.CompletionTokens, &m.TotalTokens, &m.CostUSD); err != nil {
			return nil, fmt.Errorf("scanning model row: %w", err)
		}
		r.ByModel = append(r.ByModel, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating rows: %w", err)
	}

	return r, nil
}

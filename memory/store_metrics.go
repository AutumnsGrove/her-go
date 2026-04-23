package memory

import (
	"fmt"
	"time"
)

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
// If messageID is 0, it's stored as NULL (e.g., for agent calls).
// isFallback is true when the primary model failed and the fallback
// model handled the request (the "Haiku tax" — see issue #68).
func (s *Store) SaveMetric(model string, promptTokens, completionTokens, totalTokens int, costUSD float64, latencyMs int, messageID int64, isFallback bool) error {
	var msgID interface{} = messageID
	if messageID == 0 {
		msgID = nil
	}
	_, err := s.db.Exec(
		`INSERT INTO metrics (model, prompt_tokens, completion_tokens, total_tokens, cost_usd, latency_ms, message_id, is_fallback)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		model, promptTokens, completionTokens, totalTokens, costUSD, latencyMs, msgID, isFallback,
	)
	if err != nil {
		return fmt.Errorf("saving metric: %w", err)
	}
	return nil
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
func (s *Store) GetStats() (*Stats, error) {
	st := &Stats{}

	// Message counts by role.
	s.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&st.TotalMessages)
	s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE role = 'user'`).Scan(&st.UserMessages)
	s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE role = 'assistant'`).Scan(&st.MiraMessages)

	// Fact counts by subject.
	s.db.QueryRow(`SELECT COUNT(*) FROM facts WHERE active = 1`).Scan(&st.TotalFacts)
	s.db.QueryRow(`SELECT COUNT(*) FROM facts WHERE active = 1 AND COALESCE(subject, 'user') = 'user'`).Scan(&st.UserFacts)
	s.db.QueryRow(`SELECT COUNT(*) FROM facts WHERE active = 1 AND COALESCE(subject, 'user') = 'self'`).Scan(&st.SelfFacts)

	// Token + cost totals, split by chat vs agent model.
	// Chat models have latency_ms > 0 (agent calls log latency as 0).
	s.db.QueryRow(`SELECT COALESCE(SUM(total_tokens), 0), COALESCE(SUM(cost_usd), 0) FROM metrics`).Scan(&st.TotalTokens, &st.TotalCostUSD)
	s.db.QueryRow(`SELECT COALESCE(SUM(total_tokens), 0), COALESCE(SUM(cost_usd), 0) FROM metrics WHERE latency_ms > 0`).Scan(&st.ChatTokens, &st.ChatCostUSD)
	s.db.QueryRow(`SELECT COALESCE(SUM(total_tokens), 0), COALESCE(SUM(cost_usd), 0) FROM metrics WHERE latency_ms = 0`).Scan(&st.AgentTokens, &st.AgentCostUSD)

	// Average chat latency (exclude agent calls which have 0 latency).
	s.db.QueryRow(`SELECT COALESCE(AVG(latency_ms), 0) FROM metrics WHERE latency_ms > 0`).Scan(&st.AvgLatencyMs)

	// Distinct days with messages (gives a sense of how many days active).
	s.db.QueryRow(`SELECT COUNT(DISTINCT DATE(timestamp)) FROM messages`).Scan(&st.ConversationDays)

	// Command usage from the command_log table.
	s.db.QueryRow(`SELECT COUNT(*) FROM command_log`).Scan(&st.TotalCommands)
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

// UsageReport bundles everything the `her usage` command needs.
type UsageReport struct {
	Periods []PeriodUsage
	ByModel []ModelUsage
}

// GetUsageReport builds a complete cost/token breakdown.
// Queries the metrics table with different time windows and a per-model
// GROUP BY. Each query is small and fast — SQLite handles this easily
// at our scale.
func (s *Store) GetUsageReport() (*UsageReport, error) {
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

	return r, nil
}

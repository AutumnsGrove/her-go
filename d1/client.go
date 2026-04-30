// Package d1 provides a thin HTTP client for Cloudflare D1's REST API.
//
// D1 is serverless SQLite running on Cloudflare's edge network. We talk to
// it over HTTP rather than through a Workers binding, since the bot runs on
// local machines (Mac Mini / MacBook), not inside a Worker.
//
// The client supports two operations:
//   - Query: execute a single SQL statement and get rows back
//   - Batch: execute multiple statements in one round-trip (transactional)
//
// This is intentionally minimal — just enough for the sync layer to push
// writes and pull rows. Think of it like Python's requests.post() but
// specialized for one API.
package d1

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"her/logger"
)

var log = logger.WithPrefix("d1")

// Client talks to a single D1 database via the Cloudflare REST API.
// Safe for concurrent use — the underlying http.Client handles connection
// pooling and goroutine safety (same as Python's requests.Session).
type Client struct {
	accountID  string
	databaseID string
	apiToken   string
	httpClient *http.Client

	// baseURL is the Cloudflare API root. Exposed for testing — in production
	// this is always "https://api.cloudflare.com/client/v4".
	baseURL string
}

// NewClient creates a D1 client for the given database.
// Returns nil if databaseID is empty (sync disabled).
func NewClient(accountID, databaseID, apiToken string) *Client {
	if databaseID == "" {
		return nil
	}
	return &Client{
		accountID:  accountID,
		databaseID: databaseID,
		apiToken:   apiToken,
		baseURL:    "https://api.cloudflare.com/client/v4",
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// ---------------------------------------------------------------------------
// Public types
// ---------------------------------------------------------------------------

// Statement is a single SQL statement with optional positional parameters.
// Parameters use ? placeholders, same as SQLite's sqlite3_bind.
//
//	d1.Statement{SQL: "INSERT INTO memories (memory, category) VALUES (?, ?)", Params: []any{"cats are cute", "pets"}}
type Statement struct {
	SQL    string `json:"sql"`
	Params []any  `json:"params,omitempty"`
}

// Row is a single result row — column names map to values.
// Values come back as json.Number, string, bool, or nil (for NULL).
// This is the same shape as Python's cursor.fetchone() returning a dict.
type Row map[string]any

// QueryResult holds the result of a single SQL statement.
type QueryResult struct {
	Results []Row  `json:"results"`
	Success bool   `json:"success"`
	Meta    Meta   `json:"meta"`
}

// Meta contains execution metadata returned by D1 for each statement.
type Meta struct {
	Duration    float64 `json:"duration"`
	RowsRead    int     `json:"rows_read"`
	RowsWritten int     `json:"rows_written"`
	Changes     int     `json:"changes"`
	LastRowID   int64   `json:"last_row_id"`
	ChangedDB   bool    `json:"changed_db"`
	SizeAfter   int64   `json:"size_after"`
}

// ---------------------------------------------------------------------------
// Query — single statement
// ---------------------------------------------------------------------------

// Query executes a single SQL statement and returns the result rows.
// For write operations (INSERT, UPDATE, DELETE), the returned slice will
// be empty — check QueryResult.Meta for affected row counts.
//
//	rows, err := client.Query("SELECT * FROM memories WHERE id > ?", 42)
func (c *Client) Query(sql string, params ...any) (*QueryResult, error) {
	results, err := c.execute(Statement{SQL: sql, Params: params})
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("d1: empty response for query")
	}
	return &results[0], nil
}

// ---------------------------------------------------------------------------
// Batch — multiple statements, one round-trip
// ---------------------------------------------------------------------------

// Batch executes multiple SQL statements in a single HTTP request.
// D1 runs them as a transaction — all succeed or all fail. Returns one
// QueryResult per statement, in the same order as the input.
//
// This is the workhorse for the push layer: batch several INSERT OR REPLACE
// statements together instead of one HTTP call per row.
func (c *Client) Batch(stmts []Statement) ([]QueryResult, error) {
	if len(stmts) == 0 {
		return nil, nil
	}
	return c.execute(stmts...)
}

// ---------------------------------------------------------------------------
// Internal HTTP plumbing
// ---------------------------------------------------------------------------

// apiURL returns the full D1 query endpoint URL.
func (c *Client) apiURL() string {
	return fmt.Sprintf("%s/accounts/%s/d1/database/%s/query",
		c.baseURL, c.accountID, c.databaseID)
}

// cfResponse is the outer Cloudflare API envelope.
// D1 results live inside result[] — one entry per SQL statement.
type cfResponse struct {
	Success bool          `json:"success"`
	Errors  []cfError     `json:"errors"`
	Result  []QueryResult `json:"result"`
}

type cfError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// execute sends one or more statements to D1 and returns the results.
// A single statement is sent as {"sql": "...", "params": [...]}.
// Multiple statements use {"batch": [{...}, {...}]}.
func (c *Client) execute(stmts ...Statement) ([]QueryResult, error) {
	// Build the request body. D1 uses different shapes for single vs batch.
	var body any
	if len(stmts) == 1 {
		body = stmts[0]
	} else {
		// Batch mode — wrap in {"batch": [...]}
		body = map[string]any{"batch": stmts}
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshaling d1 request: %w", err)
	}

	req, err := http.NewRequest("POST", c.apiURL(), bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("creating d1 request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("d1 http request: %w", err)
	}
	defer resp.Body.Close()

	// Read the full body — D1 responses are small enough to buffer.
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading d1 response: %w", err)
	}

	// Check HTTP status first. D1 returns 200 for successful queries,
	// even if individual statements within a batch fail.
	if resp.StatusCode != http.StatusOK {
		// Try to extract the error message from the CF envelope.
		var cfResp cfResponse
		if json.Unmarshal(respBody, &cfResp) == nil && len(cfResp.Errors) > 0 {
			return nil, fmt.Errorf("d1 api error (%d): %s", resp.StatusCode, cfResp.Errors[0].Message)
		}
		return nil, fmt.Errorf("d1 api error (%d): %s", resp.StatusCode, string(respBody))
	}

	// Parse the response envelope.
	var cfResp cfResponse
	if err := json.Unmarshal(respBody, &cfResp); err != nil {
		return nil, fmt.Errorf("unmarshaling d1 response: %w", err)
	}

	if !cfResp.Success {
		if len(cfResp.Errors) > 0 {
			return nil, fmt.Errorf("d1 query failed: %s", cfResp.Errors[0].Message)
		}
		return nil, fmt.Errorf("d1 query failed (no error details)")
	}

	return cfResp.Result, nil
}

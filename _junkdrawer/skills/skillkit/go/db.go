package skillkit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

// DBClient provides database access through the harness DB proxy.
//
// Skills use this to read and write database tables. Under the hood, every
// call becomes an HTTP request to the DB proxy running on localhost. The
// proxy enforces access control based on the skill's trust tier and declared
// permissions — the skill doesn't need to worry about this.
//
// This is the database equivalent of HTTPClient() — it reads an env var
// (DB_PROXY_URL instead of HTTP_PROXY) and transparently routes requests.
//
// Usage:
//
//	db := skillkit.DB()
//	result, err := db.Query("expenses", QueryParams{Where: "amount > 50"})
//	id, err := db.Insert("mood_entries", map[string]any{"rating": 4})
type DBClient struct {
	baseURL string
	client  *http.Client
}

// QueryParams configures a Query call.
//
// All fields are optional. Defaults: no filter, limit 100, offset 0.
type QueryParams struct {
	Where  string // SQL WHERE clause (e.g., "amount > 50 AND category = 'food'")
	Limit  int    // max rows to return (default 100, max 1000)
	Offset int    // skip this many rows (default 0)
}

// QueryResult is the response from a Query call.
type QueryResult struct {
	Rows   []map[string]any `json:"rows"`
	Count  int              `json:"count"`
	Limit  int              `json:"limit"`
	Offset int              `json:"offset"`
}

// WriteResult is the response from Insert, Update, or Delete.
type WriteResult struct {
	ID           int64 `json:"id,omitempty"` // new row ID (Insert only)
	RowsAffected int64 `json:"rows_affected"`
}

// DB returns a client for reading/writing database tables through the
// harness DB proxy.
//
// The client reads DB_PROXY_URL from the environment — the harness sets
// this before launching the skill, just like HTTP_PROXY for network access.
//
// If DB_PROXY_URL is not set, all methods will return errors. This means
// the skill wasn't granted db permissions in its skill.md.
func DB() *DBClient {
	return &DBClient{
		baseURL: os.Getenv("DB_PROXY_URL"),
		// Don't use HTTPClient() here — that goes through HTTP_PROXY
		// (the network proxy). The DB proxy is on localhost and should
		// be called directly. A plain http.Client with a timeout is fine.
		client: &http.Client{Timeout: 30 * 1e9}, // 30 seconds
	}
}

// Query reads rows from a table with optional filtering and pagination.
//
// Example:
//
//	result, err := db.Query("expenses", QueryParams{
//	    Where:  "category = 'groceries' AND amount > 20",
//	    Limit:  10,
//	})
//	for _, row := range result.Rows {
//	    fmt.Println(row["vendor"], row["amount"])
//	}
func (c *DBClient) Query(table string, params ...QueryParams) (*QueryResult, error) {
	if err := c.checkReady(); err != nil {
		return nil, err
	}

	// Build URL with query parameters.
	req, err := http.NewRequest("GET", c.baseURL+"/db/"+table, nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}

	if len(params) > 0 {
		p := params[0]
		q := req.URL.Query()
		if p.Where != "" {
			q.Set("where", p.Where)
		}
		if p.Limit > 0 {
			q.Set("limit", fmt.Sprintf("%d", p.Limit))
		}
		if p.Offset > 0 {
			q.Set("offset", fmt.Sprintf("%d", p.Offset))
		}
		req.URL.RawQuery = q.Encode()
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.readError(resp)
	}

	var result QueryResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &result, nil
}

// Insert adds a row to a table. Returns the new row's ID.
//
// Example:
//
//	id, err := db.Insert("mood_entries", map[string]any{
//	    "rating": 4,
//	    "note":   "good day",
//	    "source": "manual",
//	})
func (c *DBClient) Insert(table string, row map[string]any) (int64, error) {
	if err := c.checkReady(); err != nil {
		return 0, err
	}

	result, err := c.doWrite("POST", "/db/"+table, row)
	if err != nil {
		return 0, err
	}
	return result.ID, nil
}

// Update modifies a row by ID. Only include the fields you want to change.
//
// Example:
//
//	err := db.Update("expenses", "7", map[string]any{"note": "updated"})
func (c *DBClient) Update(table, id string, fields map[string]any) error {
	if err := c.checkReady(); err != nil {
		return err
	}

	_, err := c.doWrite("PUT", "/db/"+table+"/"+id, fields)
	return err
}

// Delete removes a row by ID.
//
// Example:
//
//	err := db.Delete("expenses", "7")
func (c *DBClient) Delete(table, id string) error {
	if err := c.checkReady(); err != nil {
		return err
	}

	_, err := c.doWrite("DELETE", "/db/"+table+"/"+id, nil)
	return err
}

// doWrite sends a POST, PUT, or DELETE request with an optional JSON body.
func (c *DBClient) doWrite(method, path string, body map[string]any) (*WriteResult, error) {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encoding body: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, c.baseURL+path, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	// Accept both 200 and 201 as success (Insert returns 201).
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, c.readError(resp)
	}

	var result WriteResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &result, nil
}

// checkReady returns an error if the client isn't configured.
func (c *DBClient) checkReady() error {
	if c.baseURL == "" {
		return fmt.Errorf("DB_PROXY_URL not set — skill has no database permissions")
	}
	return nil
}

// readError extracts an error message from a non-200 HTTP response.
func (c *DBClient) readError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)

	// Try to extract the error message from JSON.
	var errResp struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &errResp) == nil && errResp.Error != "" {
		return fmt.Errorf("db proxy: %s (status %d)", errResp.Error, resp.StatusCode)
	}

	return fmt.Errorf("db proxy: status %d: %s", resp.StatusCode, string(body))
}

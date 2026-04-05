// Package integrate provides thin REST API wrappers for external services.
// Each integration follows the same pattern: a Client struct with an API key
// and http.Client, methods that return Go structs, and formatting helpers
// that produce markdown strings for injection into agent context.
//
// These are intentionally minimal — just enough to expose the service's
// features as agent tools. No caching, no background sync, no local state.
package integrate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// TodoistClient wraps the Todoist REST API v2 for task management.
//
// Todoist uses Bearer token auth — get your API token from
// https://app.todoist.com/app/settings/integrations/developer
//
// The REST API v2 docs: https://developer.todoist.com/rest/v2
type TodoistClient struct {
	apiKey string
	http   *http.Client
}

// NewTodoistClient creates a client for the Todoist REST API.
// Returns nil if apiKey is empty (not configured).
func NewTodoistClient(apiKey string) *TodoistClient {
	if apiKey == "" {
		return nil
	}
	return &TodoistClient{
		apiKey: apiKey,
		http:   &http.Client{Timeout: 15 * time.Second},
	}
}

// baseURL is the Todoist REST API v2 root. Unlike OpenRouter or Tavily,
// this never changes — there's no self-hosted Todoist.
const baseURL = "https://api.todoist.com/rest/v2"

// ---------------------------------------------------------------------------
// Types — these map directly to the Todoist API JSON responses.
// Only the fields we actually use are declared. Go's JSON decoder
// silently ignores unknown fields, so we don't need to map everything.
// ---------------------------------------------------------------------------

// Task represents a Todoist task. The JSON tags match the API response
// field names exactly — this is how Go does JSON deserialization
// (like Python's dataclass with field aliases).
type Task struct {
	ID          string   `json:"id"`
	Content     string   `json:"content"`      // task title
	Description string   `json:"description"`  // optional longer description
	Priority    int      `json:"priority"`      // 1 (normal) to 4 (urgent) — Todoist inverts this in the UI
	Due         *DueDate `json:"due"`           // nil if no due date
	ProjectID   string   `json:"project_id"`
	Labels      []string `json:"labels"`
	IsCompleted bool     `json:"is_completed"`
	CreatedAt   string   `json:"created_at"`
	URL         string   `json:"url"` // link to task in Todoist app
}

// DueDate holds a task's due date info. Todoist supports both date-only
// ("2026-04-05") and datetime ("2026-04-05T14:00:00Z") due values.
// The "string" field is the human-readable version ("Apr 5", "today").
type DueDate struct {
	Date      string `json:"date"`       // YYYY-MM-DD or datetime
	String    string `json:"string"`     // human-readable ("today", "every monday")
	IsRecurring bool `json:"is_recurring"`
}

// Project represents a Todoist project.
type Project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ---------------------------------------------------------------------------
// API methods
// ---------------------------------------------------------------------------

// ListTasks returns active (non-completed) tasks, optionally filtered.
// The filter parameter uses Todoist's filter syntax:
//   - "today" — tasks due today
//   - "overdue" — overdue tasks
//   - "today | overdue" — both
//   - "#ProjectName" — tasks in a specific project
//   - "" — all active tasks
//
// This is a GET request with query parameters — different from Tavily's
// POST-based API. In Go, url.Values handles query string encoding, but
// for simple cases like this, string concatenation works fine.
func (c *TodoistClient) ListTasks(filter string) ([]Task, error) {
	endpoint := baseURL + "/tasks"
	if filter != "" {
		// URL-encode the filter — Todoist filter syntax can contain pipes,
		// spaces, and special chars (e.g., "today | overdue").
		endpoint += "?filter=" + url.QueryEscape(filter)
	}

	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	c.setAuth(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("listing tasks: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("todoist error (status %d): %s", resp.StatusCode, string(body))
	}

	var tasks []Task
	if err := json.Unmarshal(body, &tasks); err != nil {
		return nil, fmt.Errorf("parsing tasks: %w", err)
	}

	return tasks, nil
}

// createTaskRequest is the JSON body for POST /tasks.
type createTaskRequest struct {
	Content     string   `json:"content"`
	Description string   `json:"description,omitempty"`
	DueString   string   `json:"due_string,omitempty"` // natural language: "tomorrow", "every monday"
	Priority    int      `json:"priority,omitempty"`    // 1-4 (4 = urgent)
	ProjectID   string   `json:"project_id,omitempty"`
	Labels      []string `json:"labels,omitempty"`
}

// CreateTask creates a new task in Todoist.
// dueString uses Todoist's natural language parsing — "tomorrow",
// "next monday", "every day at 9am" all work.
func (c *TodoistClient) CreateTask(content, description, dueString string, priority int, projectID string, labels []string) (*Task, error) {
	reqBody := createTaskRequest{
		Content:     content,
		Description: description,
		DueString:   dueString,
		Priority:    priority,
		ProjectID:   projectID,
		Labels:      labels,
	}

	jsonBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	req, err := http.NewRequest("POST", baseURL+"/tasks", bytes.NewReader(jsonBytes))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	c.setAuth(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("creating task: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("todoist error (status %d): %s", resp.StatusCode, string(body))
	}

	var task Task
	if err := json.Unmarshal(body, &task); err != nil {
		return nil, fmt.Errorf("parsing task: %w", err)
	}

	return &task, nil
}

// updateTaskRequest is the JSON body for POST /tasks/{id}.
// All fields are pointers so we can distinguish "not set" (nil) from
// "set to zero value". This is a common Go pattern for PATCH-style APIs
// where you only want to send the fields that changed.
type updateTaskRequest struct {
	Content     *string  `json:"content,omitempty"`
	Description *string  `json:"description,omitempty"`
	DueString   *string  `json:"due_string,omitempty"`
	Priority    *int     `json:"priority,omitempty"`
	Labels      []string `json:"labels,omitempty"`
}

// UpdateTask updates an existing task. Only non-nil fields are sent.
func (c *TodoistClient) UpdateTask(taskID string, content, description, dueString *string, priority *int, labels []string) (*Task, error) {
	reqBody := updateTaskRequest{
		Content:     content,
		Description: description,
		DueString:   dueString,
		Priority:    priority,
		Labels:      labels,
	}

	jsonBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	req, err := http.NewRequest("POST", baseURL+"/tasks/"+taskID, bytes.NewReader(jsonBytes))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	c.setAuth(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("updating task: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("todoist error (status %d): %s", resp.StatusCode, string(body))
	}

	var task Task
	if err := json.Unmarshal(body, &task); err != nil {
		return nil, fmt.Errorf("parsing task: %w", err)
	}

	return &task, nil
}

// CompleteTask marks a task as completed. Todoist calls this "closing"
// a task. For recurring tasks, this completes the current occurrence
// and schedules the next one automatically.
//
// Returns nil on success — the API returns 204 No Content.
func (c *TodoistClient) CompleteTask(taskID string) error {
	req, err := http.NewRequest("POST", baseURL+"/tasks/"+taskID+"/close", nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	c.setAuth(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("completing task: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("todoist error (status %d): %s", resp.StatusCode, string(body))
	}

	return nil
}

// ListProjects returns all projects. Useful for resolving project names
// to IDs when creating tasks.
func (c *TodoistClient) ListProjects() ([]Project, error) {
	req, err := http.NewRequest("GET", baseURL+"/projects", nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	c.setAuth(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("listing projects: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("todoist error (status %d): %s", resp.StatusCode, string(body))
	}

	var projects []Project
	if err := json.Unmarshal(body, &projects); err != nil {
		return nil, fmt.Errorf("parsing projects: %w", err)
	}

	return projects, nil
}

// ---------------------------------------------------------------------------
// Formatting helpers — produce markdown for agent context
// ---------------------------------------------------------------------------

// FormatTasks turns a task list into a readable markdown string for
// injection into the agent context. Includes due dates, priority
// indicators, and task IDs (needed for complete/update operations).
func FormatTasks(tasks []Task) string {
	if len(tasks) == 0 {
		return "No tasks found."
	}

	var b strings.Builder
	for _, t := range tasks {
		// Priority indicator: Todoist uses 1=normal, 4=urgent.
		// We show flags for elevated priority so the agent notices them.
		pri := ""
		switch t.Priority {
		case 4:
			pri = " !!!"
		case 3:
			pri = " !!"
		case 2:
			pri = " !"
		}

		fmt.Fprintf(&b, "- [ID=%s] %s%s", t.ID, t.Content, pri)

		if t.Due != nil {
			fmt.Fprintf(&b, " (due: %s", t.Due.Date)
			if t.Due.IsRecurring {
				b.WriteString(", recurring")
			}
			b.WriteString(")")
		}

		if t.Description != "" {
			fmt.Fprintf(&b, "\n  %s", t.Description)
		}

		if len(t.Labels) > 0 {
			fmt.Fprintf(&b, "\n  labels: %s", strings.Join(t.Labels, ", "))
		}

		b.WriteString("\n")
	}

	return b.String()
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// setAuth adds the Authorization header to a request.
func (c *TodoistClient) setAuth(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
}

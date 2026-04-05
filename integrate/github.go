// integrate/github.go — thin wrapper around the GitHub REST API for
// issue management. Same pattern as todoist.go: stateless client,
// constructed on-demand from config, returns Go structs + markdown helpers.
//
// GitHub REST API docs: https://docs.github.com/en/rest/issues
// Auth: personal access token (classic) with "repo" scope, or
// fine-grained token with Issues read/write permission.
package integrate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// GitHubClient wraps the GitHub REST API for issue operations.
// Scoped to a configured set of repos — the agent can only touch
// repositories explicitly listed in config.yaml.
type GitHubClient struct {
	token string
	repos []string // allowed repos in "owner/repo" format
	http  *http.Client
}

// NewGitHubClient creates a client for the GitHub REST API.
// Returns nil if token is empty (not configured).
// repos is the list of allowed "owner/repo" strings from config.
func NewGitHubClient(token string, repos []string) *GitHubClient {
	if token == "" {
		return nil
	}
	return &GitHubClient{
		token: token,
		repos: repos,
		http:  &http.Client{Timeout: 15 * time.Second},
	}
}

// githubBaseURL is the GitHub REST API root.
const githubBaseURL = "https://api.github.com"

// ---------------------------------------------------------------------------
// Types — fields we care about from the GitHub API responses.
// GitHub returns a LOT of fields per issue; we only map the useful ones.
// ---------------------------------------------------------------------------

// Issue represents a GitHub issue (or pull request — GitHub's API treats
// PRs as a superset of issues, but we filter them out).
type Issue struct {
	Number    int          `json:"number"`
	Title     string       `json:"title"`
	Body      string       `json:"body"`
	State     string       `json:"state"` // "open" or "closed"
	Labels    []Label      `json:"labels"`
	Assignees []User       `json:"assignees"`
	User      User         `json:"user"`      // who created it
	HTMLURL   string       `json:"html_url"`  // link to view in browser
	CreatedAt string       `json:"created_at"`
	UpdatedAt string       `json:"updated_at"`
	PullReq   *PullReqRef  `json:"pull_request"` // non-nil = this is a PR, not an issue
}

// Label is a GitHub issue label.
type Label struct {
	Name string `json:"name"`
}

// User is a minimal GitHub user reference.
type User struct {
	Login string `json:"login"`
}

// PullReqRef exists on issues that are actually pull requests.
// We use its presence to filter PRs out of issue lists.
type PullReqRef struct {
	URL string `json:"url"`
}

// ---------------------------------------------------------------------------
// API methods
// ---------------------------------------------------------------------------

// RepoAllowed checks if the given "owner/repo" string is in the configured
// allow-list. This prevents the agent from accessing repos the user hasn't
// explicitly permitted.
func (c *GitHubClient) RepoAllowed(repo string) bool {
	for _, r := range c.repos {
		if strings.EqualFold(r, repo) {
			return true
		}
	}
	return false
}

// AllowedRepos returns the configured repo list so the agent can show
// the user which repos are available.
func (c *GitHubClient) AllowedRepos() []string {
	return c.repos
}

// ListIssues returns open issues for a repo. Only returns actual issues,
// not pull requests (GitHub's API mixes them together).
//
// state can be "open", "closed", or "all". Empty defaults to "open".
// labels is a comma-separated list of label names to filter by (optional).
func (c *GitHubClient) ListIssues(repo, state, labels string, limit int) ([]Issue, error) {
	if !c.RepoAllowed(repo) {
		return nil, fmt.Errorf("repo %q is not in the configured allow-list (allowed: %s)", repo, strings.Join(c.repos, ", "))
	}

	if state == "" {
		state = "open"
	}
	if limit <= 0 || limit > 100 {
		limit = 30
	}

	endpoint := fmt.Sprintf("%s/repos/%s/issues?state=%s&per_page=%d&sort=updated&direction=desc",
		githubBaseURL, repo, state, limit)
	if labels != "" {
		endpoint += "&labels=" + labels
	}

	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	c.setAuth(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("listing issues: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github error (status %d): %s", resp.StatusCode, string(body))
	}

	var allIssues []Issue
	if err := json.Unmarshal(body, &allIssues); err != nil {
		return nil, fmt.Errorf("parsing issues: %w", err)
	}

	// Filter out pull requests — GitHub's issues endpoint returns both.
	// This is a known quirk of the API. We check for the presence of
	// the pull_request field (non-nil = it's a PR).
	var issues []Issue
	for _, iss := range allIssues {
		if iss.PullReq == nil {
			issues = append(issues, iss)
		}
	}

	return issues, nil
}

// createIssueRequest is the JSON body for POST /repos/{owner}/{repo}/issues.
type createIssueRequest struct {
	Title  string   `json:"title"`
	Body   string   `json:"body,omitempty"`
	Labels []string `json:"labels,omitempty"`
}

// CreateIssue files a new issue on a repo.
func (c *GitHubClient) CreateIssue(repo, title, body string, labels []string) (*Issue, error) {
	if !c.RepoAllowed(repo) {
		return nil, fmt.Errorf("repo %q is not in the configured allow-list (allowed: %s)", repo, strings.Join(c.repos, ", "))
	}

	reqBody := createIssueRequest{
		Title:  title,
		Body:   body,
		Labels: labels,
	}

	jsonBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	req, err := http.NewRequest("POST", fmt.Sprintf("%s/repos/%s/issues", githubBaseURL, repo), bytes.NewReader(jsonBytes))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	c.setAuth(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("creating issue: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	// GitHub returns 201 Created for new issues (not 200 OK).
	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("github error (status %d): %s", resp.StatusCode, string(respBody))
	}

	var issue Issue
	if err := json.Unmarshal(respBody, &issue); err != nil {
		return nil, fmt.Errorf("parsing issue: %w", err)
	}

	return &issue, nil
}

// ---------------------------------------------------------------------------
// Formatting helpers
// ---------------------------------------------------------------------------

// FormatIssues turns an issue list into readable markdown for the agent.
// Includes issue number, title, labels, and a truncated body preview.
func FormatIssues(issues []Issue, repo string) string {
	if len(issues) == 0 {
		return fmt.Sprintf("No issues found in %s.", repo)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "**%s** — %d issue(s):\n\n", repo, len(issues))

	for _, iss := range issues {
		fmt.Fprintf(&b, "- #%d: %s", iss.Number, iss.Title)

		if len(iss.Labels) > 0 {
			labelNames := make([]string, len(iss.Labels))
			for i, l := range iss.Labels {
				labelNames[i] = l.Name
			}
			fmt.Fprintf(&b, " [%s]", strings.Join(labelNames, ", "))
		}

		b.WriteString("\n")

		// Show a truncated body preview if available.
		if iss.Body != "" {
			preview := iss.Body
			if len(preview) > 120 {
				preview = preview[:120] + "..."
			}
			// Collapse newlines for compact display.
			preview = strings.ReplaceAll(preview, "\n", " ")
			fmt.Fprintf(&b, "  %s\n", preview)
		}
	}

	return b.String()
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// setAuth adds GitHub authentication headers.
func (c *GitHubClient) setAuth(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
}

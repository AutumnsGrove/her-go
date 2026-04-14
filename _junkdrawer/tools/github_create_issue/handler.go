// Package github_create_issue implements the github_create_issue tool —
// files a new issue on a configured GitHub repository.
package github_create_issue

import (
	"encoding/json"
	"fmt"
	"strings"

	"her/integrate"
	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/github_create_issue")

func init() {
	tools.Register("github_create_issue", Handle)
}

// Handle creates a new GitHub issue from the agent's arguments.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		Repo   string `json:"repo"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		Labels string `json:"labels"` // comma-separated
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	if args.Repo == "" {
		return "error: repo is required (e.g., 'AutumnsGrove/grove.place')"
	}
	if args.Title == "" {
		return "error: title is required"
	}

	client := integrate.NewGitHubClient(ctx.Cfg.GitHub.Token, ctx.Cfg.GitHub.Repos)
	if client == nil {
		return "GitHub is not configured. Add github.token and github.repos to config.yaml."
	}

	// Parse comma-separated labels into a slice.
	var labels []string
	if args.Labels != "" {
		for _, l := range strings.Split(args.Labels, ",") {
			l = strings.TrimSpace(l)
			if l != "" {
				labels = append(labels, l)
			}
		}
	}

	issue, err := client.CreateIssue(args.Repo, args.Title, args.Body, labels)
	if err != nil {
		log.Error("creating issue", "repo", args.Repo, "title", args.Title, "err", err)
		return fmt.Sprintf("error creating issue: %v", err)
	}

	log.Infof("  github_create_issue: %s #%d %q", args.Repo, issue.Number, issue.Title)

	result := fmt.Sprintf("Issue #%d created: %s\nURL: %s", issue.Number, issue.Title, issue.HTMLURL)
	if len(issue.Labels) > 0 {
		labelNames := make([]string, len(issue.Labels))
		for i, l := range issue.Labels {
			labelNames[i] = l.Name
		}
		result += fmt.Sprintf("\nLabels: %s", strings.Join(labelNames, ", "))
	}

	return result
}

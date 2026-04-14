// Package github_list_issues implements the github_list_issues tool —
// retrieves issues from a configured GitHub repository.
package github_list_issues

import (
	"encoding/json"
	"fmt"
	"strings"

	"her/integrate"
	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/github_list_issues")

func init() {
	tools.Register("github_list_issues", Handle)
}

// Handle queries GitHub for issues on a configured repo.
// If no repo is specified, returns the list of allowed repos so the
// agent knows what's available.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		Repo   string `json:"repo"`
		State  string `json:"state"`
		Labels string `json:"labels"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	client := integrate.NewGitHubClient(ctx.Cfg.GitHub.Token, ctx.Cfg.GitHub.Repos)
	if client == nil {
		return "GitHub is not configured. Add github.token and github.repos to config.yaml."
	}

	// If no repo specified, show available repos.
	if args.Repo == "" {
		repos := client.AllowedRepos()
		if len(repos) == 0 {
			return "No repos configured. Add repos to github.repos in config.yaml."
		}
		return fmt.Sprintf("Available repos:\n- %s\n\nSpecify a repo to list its issues.", strings.Join(repos, "\n- "))
	}

	issues, err := client.ListIssues(args.Repo, args.State, args.Labels, args.Limit)
	if err != nil {
		log.Error("listing issues", "repo", args.Repo, "err", err)
		return fmt.Sprintf("error listing issues: %v", err)
	}

	log.Infof("  github_list_issues: %s (state=%s) → %d issues", args.Repo, args.State, len(issues))

	return integrate.FormatIssues(issues, args.Repo)
}

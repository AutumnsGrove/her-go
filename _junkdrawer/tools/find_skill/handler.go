// Package find_skill implements the find_skill tool — searches the skill
// registry using KNN cosine similarity over description embeddings.
//
// The agent calls this with a natural language query like "get weather
// forecast" and gets back matching skills ranked by relevance. The agent
// then decides which one to call via run_skill (or none, if nothing fits).
//
// This is the discovery step in the two-step skill flow:
// find_skill (discover) → run_skill (execute)
package find_skill

import (
	"encoding/json"
	"fmt"
	"strings"

	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/find_skill")

func init() {
	tools.Register("find_skill", Handle)
}

// Handle searches the skill registry for skills matching the query.
// Returns up to 5 results with name, description, score, and parameter schema.
func Handle(argsJSON string, ctx *tools.Context) string {
	if ctx.SkillRegistry == nil {
		return "no skills available — skill registry not initialized"
	}

	var args struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing find_skill args: %s", err)
	}
	if args.Query == "" {
		return "error: query is required"
	}

	// Search with reasonable defaults: top 5 results, minimum 0.3 score.
	// 0.3 is intentionally low — we'd rather show marginal matches than
	// miss something useful. The agent can judge relevance from the scores.
	results, err := ctx.SkillRegistry.Find(args.Query, 5, 0.3)
	if err != nil {
		return fmt.Sprintf("error searching skills: %s", err)
	}

	if len(results) == 0 {
		return "no matching skills found"
	}

	// Format results for the agent. Include name, description, score,
	// and parameter summary so the agent knows how to call the skill.
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d matching skill(s):\n\n", len(results)))

	for _, r := range results {
		sb.WriteString(fmt.Sprintf("**%s** (score: %.2f)\n", r.Skill.Name, r.Score))
		sb.WriteString(fmt.Sprintf("  %s\n", r.Skill.Description))

		// Show parameter summary so the agent knows what to pass.
		if len(r.Skill.Params) > 0 {
			sb.WriteString("  params:\n")
			for _, p := range r.Skill.Params {
				req := ""
				if p.Required {
					req = " (required)"
				}
				sb.WriteString(fmt.Sprintf("    - %s (%s)%s: %s\n", p.Name, p.Type, req, p.Description))
			}
		}
		sb.WriteString("\n")
	}

	log.Infof("  find_skill: %d results for %q", len(results), args.Query)
	return sb.String()
}

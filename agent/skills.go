package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"her/skills/loader"
)

// execFindSkill handles the find_skill tool call. It searches the skill
// registry using KNN cosine similarity over description embeddings.
//
// The agent calls this with a natural language query like "get weather
// forecast" and gets back matching skills ranked by relevance. The agent
// then decides which one to call (or none, if nothing fits).
func execFindSkill(argsJSON string, tctx *toolContext) string {
	if tctx.skillRegistry == nil {
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
	// miss something useful. The agent can judge relevance from scores.
	results, err := tctx.skillRegistry.Find(args.Query, 5, 0.3)
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

	return sb.String()
}

// execRunSkill handles the run_skill tool call. It looks up the skill by
// name, executes it via the runner, and returns the result.
//
// The skill binary receives args as JSON on stdin and writes its result
// to stdout. The runner handles compilation (Go), timeouts, and
// environment sandboxing.
func execRunSkill(argsJSON string, tctx *toolContext) string {
	if tctx.skillRegistry == nil {
		return "no skills available — skill registry not initialized"
	}

	var args struct {
		Name string         `json:"name"`
		Args map[string]any `json:"args"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing run_skill args: %s", err)
	}
	if args.Name == "" {
		return "error: skill name is required"
	}

	// Look up the skill.
	skill := tctx.skillRegistry.Get(args.Name)
	if skill == nil {
		// Suggest using find_skill — common mistake is to guess a name.
		available := tctx.skillRegistry.List()
		if len(available) > 0 {
			return fmt.Sprintf("unknown skill %q. Available: %s. Use find_skill to search by intent.",
				args.Name, strings.Join(available, ", "))
		}
		return fmt.Sprintf("unknown skill %q — no skills are registered", args.Name)
	}

	// Default to empty args if none provided.
	if args.Args == nil {
		args.Args = make(map[string]any)
	}

	// Update status so the user sees what's happening.
	if tctx.statusCallback != nil {
		tctx.statusCallback(fmt.Sprintf("running %s...", skill.Name))
	}

	log.Info("running skill", "name", skill.Name)
	result, err := loader.Run(skill, args.Args)
	if err != nil {
		return fmt.Sprintf("error running skill %s: %s", skill.Name, err)
	}

	log.Info("skill finished", "name", skill.Name, "duration", result.Duration)

	// Format the result for the agent.
	if result.Error != "" {
		if result.Output != nil {
			// Skill wrote error JSON before exiting (via skillkit.Error).
			return fmt.Sprintf("skill %s error: %s\noutput: %s", skill.Name, result.Error, string(result.Output))
		}
		return fmt.Sprintf("skill %s error: %s", skill.Name, result.Error)
	}

	if result.Output != nil {
		return string(result.Output)
	}
	if result.RawOut != "" {
		return fmt.Sprintf("(non-JSON output) %s", result.RawOut)
	}

	return "skill completed with no output"
}

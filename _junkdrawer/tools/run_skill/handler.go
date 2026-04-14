// Package run_skill implements the run_skill tool — executes a skill by name
// with given arguments via the skills/loader package.
//
// The skill binary receives args as JSON on stdin and writes its result to
// stdout. The runner handles compilation (for Go skills), timeouts, and
// environment sandboxing.
package run_skill

import (
	"encoding/json"
	"fmt"
	"strings"

	"her/logger"
	"her/skills/loader"
	"her/tools"
)

var log = logger.WithPrefix("tools/run_skill")

func init() {
	tools.Register("run_skill", Handle)
}

// Handle looks up the skill by name, executes it, and returns its output.
func Handle(argsJSON string, ctx *tools.Context) string {
	if ctx.SkillRegistry == nil {
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
	skill := ctx.SkillRegistry.Get(args.Name)
	if skill == nil {
		// Suggest using find_skill — a common mistake is to guess the name.
		available := ctx.SkillRegistry.List()
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
	if ctx.StatusCallback != nil {
		ctx.StatusCallback(fmt.Sprintf("running %s...", skill.Name))
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

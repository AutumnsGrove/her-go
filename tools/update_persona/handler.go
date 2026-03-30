// Package update_persona implements the update_persona tool — rewrites the
// bot's persona.md file to reflect accumulated self-observations.
//
// This is the persona evolution mechanism: the agent calls this when self-facts
// suggest a meaningful shift in how the bot understands herself. It's called
// rarely (at most every N reflections) to prevent thrashing.
//
// The file is written with {{her}} as a placeholder for the bot's name,
// keeping persona.md portable across config changes.
package update_persona

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/update_persona")

func init() {
	tools.Register("update_persona", Handle)
}

// Handle rewrites persona.md on disk and saves a version record to the DB.
//
// The agent passes the full new persona content. Before writing, we swap the
// bot's literal name back to {{her}} so the file works as a template — if the
// name ever changes in config.yaml, the persona file doesn't need updating.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		Content string `json:"content"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	// Swap the bot's literal name back to {{her}} before writing to disk,
	// keeping the persona file as a portable template.
	personaContent := strings.ReplaceAll(args.Content, ctx.Cfg.Identity.Her, "{{her}}")

	// os.WriteFile is Go's equivalent of Python's open(path, 'w').write(content).
	// 0644 is the Unix file permission: owner can read/write, group/others read-only.
	if err := os.WriteFile(ctx.PersonaFile, []byte(personaContent), 0644); err != nil {
		return fmt.Sprintf("error writing persona file: %v", err)
	}

	// Store the raw LLM output (with literal name) in the DB for history.
	id, err := ctx.Store.SavePersonaVersion(args.Content, "agent: "+args.Reason)
	if err != nil {
		return fmt.Sprintf("persona file updated but failed to save version: %v", err)
	}

	log.Infof("  update_persona: version ID=%d, reason=%s", id, args.Reason)
	return fmt.Sprintf("persona updated (version ID=%d, reason: %s)", id, args.Reason)
}

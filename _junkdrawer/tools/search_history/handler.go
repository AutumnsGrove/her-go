// Package search_history implements the search_history tool — searches a
// skill's sidecar database for past execution results matching the query.
//
// This lets the agent check "did I already search for this?" before re-running
// a skill. Past results include freshness metadata so the agent can judge
// whether to reuse a cached result or run fresh.
//
// The sidecar database is a per-skill SQLite file that the skills/loader
// package maintains alongside each skill binary. It records inputs, outputs,
// and timestamps for each execution.
package search_history

import (
	"encoding/json"
	"fmt"
	"strings"

	"her/logger"
	"her/skills/loader"
	"her/tools"
)

var log = logger.WithPrefix("tools/search_history")

func init() {
	tools.Register("search_history", Handle)
}

// Handle embeds the query, opens the skill's sidecar DB, and runs KNN search
// over past execution results. Returns ranked results with freshness metadata.
func Handle(argsJSON string, ctx *tools.Context) string {
	if ctx.SkillRegistry == nil {
		return "no skills available — skill registry not initialized"
	}
	if ctx.EmbedClient == nil {
		return "search_history unavailable — embedding client not configured"
	}

	var args struct {
		SkillName string `json:"skill_name"`
		Query     string `json:"query"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing search_history args: %s", err)
	}
	if args.SkillName == "" || args.Query == "" {
		return "error: both skill_name and query are required"
	}

	// Look up the skill.
	skill := ctx.SkillRegistry.Get(args.SkillName)
	if skill == nil {
		return fmt.Sprintf("unknown skill %q", args.SkillName)
	}

	// 4th-party skills have no sidecar access — they're unvetted and
	// run in a more restrictive sandbox.
	if skill.TrustLevel == loader.TrustFourthParty {
		return "no history available for this skill (4th-party, unvetted)"
	}

	// Embed the query using the same model used to embed past results.
	queryVec, err := ctx.EmbedClient.Embed(args.Query)
	if err != nil {
		return fmt.Sprintf("error embedding query: %s", err)
	}

	// Open the sidecar DB and search. The dimension must match what was
	// used when the sidecar was created — that's ctx.EmbedClient.Dimension.
	sdb, err := loader.OpenSidecar(skill, ctx.EmbedClient.Dimension)
	if err != nil {
		return fmt.Sprintf("no execution history for %s", args.SkillName)
	}
	defer sdb.Close() // closes when this function returns — like Python's with statement

	results, err := sdb.SearchHistory(queryVec, 5)
	if err != nil {
		return fmt.Sprintf("error searching history: %s", err)
	}

	if len(results) == 0 {
		return fmt.Sprintf("no past executions found for %s matching %q", args.SkillName, args.Query)
	}

	// Format results for the agent with status, age, duration, and result.
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d cached result(s) for %s:\n\n", len(results), args.SkillName))

	for i, r := range results {
		status := "OK"
		if r.ExitCode != 0 {
			status = "ERROR"
		}
		sb.WriteString(fmt.Sprintf("%d. [%s] %s (took %s)\n", i+1, status, r.Age, r.Duration))
		sb.WriteString(fmt.Sprintf("   args: %s\n", r.Args))
		sb.WriteString(fmt.Sprintf("   result: %s\n\n", r.Result))
	}

	log.Infof("  search_history: %d results for %s/%q", len(results), args.SkillName, args.Query)
	return sb.String()
}

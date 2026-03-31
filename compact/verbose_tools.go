// Package compact — shared list of tools with verbose output.
//
// Both the agent action history layer and the agent compaction logic need
// to know which tools produce large results that should be truncated.
// This canonical list lives here so changes propagate to both consumers.
package compact

// VerboseTools lists tools whose results are large and low-value for
// long-term agent memory. We keep the tool name and args (so the agent
// knows it searched for X) but aggressively truncate the result.
var VerboseTools = map[string]bool{
	"web_search":      true,
	"book_search":     true,
	"find_skill":      true,
	"recall_memories": true,
	"search_history":  true,
	"query_expenses":  true,
}

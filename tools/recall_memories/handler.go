// Package recall_memories implements the recall_memories tool — searches stored
// facts by semantic similarity.
//
// The agent calls this when the user asks "do you remember..." or references
// something from a past conversation. It embeds the query and runs a KNN
// search against the facts table's vector index.
//
// This is an active recall tool — different from the automatic context injection
// that happens at the start of every turn. That injects facts silently; this
// tool is for when the agent needs to explicitly look something up.
package recall_memories

import (
	"encoding/json"
	"fmt"
	"strings"

	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/recall_memories")

func init() {
	tools.Register("recall_memories", Handle)
}

// Handle embeds the query and searches the facts vector index for matches.
// Returns facts sorted by similarity with distance scores so the agent can
// judge relevance. Cosine distance 0 = identical, 1 = orthogonal.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	if ctx.EmbedClient == nil {
		return "memory search is not available (embedding client not configured)"
	}
	if ctx.Store.EmbedDimension == 0 {
		return "memory search is not available (vector index not configured)"
	}

	// Default and cap the limit to avoid flooding the context window.
	if args.Limit <= 0 || args.Limit > 10 {
		args.Limit = 5
	}

	// Embed the query and search. EmbedClient.Embed returns a []float32
	// vector; SemanticSearch runs KNN against sqlite-vec.
	queryVec, err := ctx.EmbedClient.Embed(args.Query)
	if err != nil {
		// Embedding failed — server may be down. Fall back to keyword search
		// so recall still works, just less precise. Agent sees the degraded flag.
		log.Warn("embed unavailable, falling back to keyword search", "err", err)
		memories, ftsErr := ctx.Store.FindMemoriesByKeyword(args.Query)
		if ftsErr != nil || len(memories) == 0 {
			return "memory search temporarily unavailable (embed server down)"
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Found %d memories (degraded: embed server unavailable — keyword search only):\n\n", len(memories))
		for _, m := range memories {
			fmt.Fprintf(&b, "- [ID=%d, %s] %s\n", m.ID, m.Category, m.Content)
		}
		return b.String()
	}

	facts, err := ctx.Store.SemanticSearch(queryVec, args.Limit)
	if err != nil {
		return fmt.Sprintf("error searching memories: %v", err)
	}

	if len(facts) == 0 {
		return "no matching memories found"
	}

	// Format results for the agent. Include distance so it can judge relevance.
	// We convert distance to similarity (1 - distance) for readability —
	// 95% similarity is easier to reason about than 0.05 distance.
	var b strings.Builder
	fmt.Fprintf(&b, "Found %d matching memories:\n\n", len(facts))
	for _, f := range facts {
		similarity := 1 - f.Distance
		fmt.Fprintf(&b, "- [ID=%d, %s, similarity=%.0f%%] %s\n",
			f.ID, f.Category, similarity*100, f.Content)
	}

	log.Infof("  recall_memories: %d results for %q", len(facts), args.Query)
	return b.String()
}

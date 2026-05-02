// Package recall_memories implements the recall_memories tool — searches stored
// memories by semantic similarity.
//
// The agent calls this when the user asks "do you remember..." or references
// something from a past conversation. It embeds the query and runs a KNN
// search against the memories table's vector index.
//
// This is an active recall tool — different from the automatic context injection
// that happens at the start of every turn. That injects memories silently; this
// tool is for when the agent needs to explicitly look something up.
package recall_memories

import (
	"encoding/json"
	"fmt"
	"strings"

	"her/logger"
	"her/memory"
	"her/tools"
)

var log = logger.WithPrefix("tools/recall_memories")

func init() {
	tools.Register("recall_memories", Handle)
}

// Handle embeds the query and searches the memories vector index for matches.
// Returns memories sorted by similarity with distance scores so the agent can
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
	if ctx.Store.GetEmbedDimension() == 0 {
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
		var userMems, selfMems []memory.Memory
		for _, m := range memories {
			if m.Subject == "self" {
				selfMems = append(selfMems, m)
			} else {
				userMems = append(userMems, m)
			}
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Found %d memories (degraded: keyword search only):\n", len(memories))
		writeKeywordSection(&b, "About the user", userMems)
		writeKeywordSection(&b, "About myself", selfMems)
		return b.String()
	}

	memories, err := ctx.Store.SemanticSearch(queryVec, args.Limit)
	if err != nil {
		return fmt.Sprintf("error searching memories: %v", err)
	}

	if len(memories) == 0 {
		return "no matching memories found"
	}

	// Split results by subject so the agent can distinguish user facts from
	// self-observations. Without this, "I like dry humor" (self) and
	// "Autumn likes dry humor" (user) look identical in the output.
	var userMems, selfMems []memory.Memory
	for _, m := range memories {
		if m.Subject == "self" {
			selfMems = append(selfMems, m)
		} else {
			userMems = append(userMems, m)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Found %d matching memories:\n", len(memories))
	writeSection(&b, "About the user", userMems)
	writeSection(&b, "About myself", selfMems)

	log.Infof("  recall_memories: %d results for %q (user=%d, self=%d)",
		len(memories), args.Query, len(userMems), len(selfMems))
	return b.String()
}

// writeSection formats a labeled group of memories from semantic search.
// Skips the section entirely if there are no memories of that type.
// Includes similarity scores (1 - cosine distance) so the agent can judge
// relevance — 95% similarity is easier to reason about than 0.05 distance.
func writeSection(b *strings.Builder, heading string, mems []memory.Memory) {
	if len(mems) == 0 {
		return
	}
	fmt.Fprintf(b, "\n## %s\n", heading)
	for _, m := range mems {
		similarity := 1 - m.Distance
		fmt.Fprintf(b, "- [ID=%d, %s, similarity=%.0f%%] %s\n",
			m.ID, m.Category, similarity*100, m.Content)
	}
}

// writeKeywordSection formats a labeled group of memories from keyword fallback.
// No similarity scores available — keyword search doesn't produce distances.
func writeKeywordSection(b *strings.Builder, heading string, mems []memory.Memory) {
	if len(mems) == 0 {
		return
	}
	fmt.Fprintf(b, "\n## %s\n", heading)
	for _, m := range mems {
		fmt.Fprintf(b, "- [ID=%d, %s] %s\n", m.ID, m.Category, m.Content)
	}
}

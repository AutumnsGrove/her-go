// Package recall_memories implements the recall_memories tool — searches stored
// memories by semantic similarity.
//
// This is the ONLY path memories reach the chat model. The agent calls this,
// evaluates results, and passes relevant ones to reply(memories=[...]). There
// is no automatic injection — if the agent skips recall, the chat model has
// zero memory context.
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

// Handle is dual-mode: LOOKUP (id/slug — a direct, deterministic DB fetch,
// no embedding call) or SEARCH (query — semantic KNN, the fallback for when
// the agent doesn't already know which memory it wants). See
// docs/plans/PLAN-memory-hooks-dual-recall.md.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		ID       int64  `json:"id"`
		Slug     string `json:"slug"`
		Query    string `json:"query"`
		CardSlug string `json:"card_slug"`
		Limit    int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	if args.ID != 0 || args.Slug != "" {
		return handleLookup(args.ID, args.Slug, ctx)
	}

	if args.Query == "" {
		return "error: provide either id, slug, or query"
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

	// Resolve optional card_slug for scoped search.
	var cardID int64
	if args.CardSlug != "" {
		card, err := ctx.Store.GetCard(args.CardSlug)
		if err != nil {
			return fmt.Sprintf("error looking up card %q: %v", args.CardSlug, err)
		}
		if card == nil {
			return fmt.Sprintf("error: no card found with slug %q", args.CardSlug)
		}
		cardID = card.ID
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
			} else if !ctx.SelfOnly {
				userMems = append(userMems, m)
			}
		}
		filtered := len(selfMems) + len(userMems)
		var b strings.Builder
		fmt.Fprintf(&b, "Found %d memories (degraded: keyword search only):\n", filtered)
		writeKeywordSection(&b, "About the user", userMems)
		writeKeywordSection(&b, "About myself", selfMems)
		return b.String()
	}

	// Use card-scoped or subject-scoped search when narrowing is requested.
	var memories []memory.Memory
	if cardID > 0 {
		memories, err = ctx.Store.SemanticSearchByCard(queryVec, cardID, args.Limit)
	} else if ctx.SelfOnly {
		memories, err = ctx.Store.SemanticSearchBySubject(queryVec, "self", args.Limit)
	} else {
		memories, err = ctx.Store.SemanticSearch(queryVec, args.Limit)
	}
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

// handleLookup fetches a single memory by id or slug — a direct DB read, no
// embedding call, no ranking. This is the primary path once a card/self
// index hook has already told the caller which memory it wants; SEARCH mode
// in Handle is the fallback for when it hasn't.
func handleLookup(id int64, slug string, ctx *tools.Context) string {
	var m *memory.Memory
	var err error
	var by string
	if id != 0 {
		m, err = ctx.Store.GetMemory(id)
		by = fmt.Sprintf("id=%d", id)
	} else {
		m, err = ctx.Store.GetMemoryBySlug(slug)
		by = fmt.Sprintf("slug=%q", slug)
	}
	if err != nil {
		return fmt.Sprintf("error looking up memory (%s): %v", by, err)
	}
	if m == nil || !m.Active {
		return fmt.Sprintf("no active memory found for %s — try recall_memories(query=...) instead", by)
	}
	// SelfOnly guard — the introspection agent should only read self-memories.
	// Same rule update_memory enforces for writes.
	if ctx.SelfOnly && m.Subject != "self" {
		return fmt.Sprintf("rejected: memory (%s) is a %s memory, not a self-memory", by, m.Subject)
	}

	heading := "About the user"
	if m.Subject == "self" {
		heading = "About myself"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## %s\n- [ID=%d, %s] %s\n", heading, m.ID, m.Category, m.Content)
	if m.Hook != "" {
		fmt.Fprintf(&b, "  (hook: %s)\n", m.Hook)
	}

	log.Infof("  recall_memories: lookup %s → ID=%d", by, m.ID)
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

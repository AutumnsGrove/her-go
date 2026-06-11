// Package persona — memory_dreamer.go runs autonomous memory card review
// as Step 0 of the nightly dream cycle.
//
// The memory dreamer is a tool-calling agent that reviews all memory cards,
// checks recent changes via the memory log, and decides which cards to
// rewrite, merge, split, or expire. It runs BEFORE persona reflection so
// the reflection sees clean, consolidated data.
package persona

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	engine "her/agent_engine"
	"her/config"
	"her/embed"
	"her/llm"
	"her/memory"
	"her/tools"
	"her/tui"

	// Blank imports register the tool handlers the dreamer uses.
	_ "her/tools/create_card"
	_ "her/tools/done"
	_ "her/tools/list_cards"
	_ "her/tools/merge_memories"
	_ "her/tools/read_card"
	_ "her/tools/remove_memory"
	_ "her/tools/think"
	_ "her/tools/update_card"
)

//go:embed memory_dreamer_prompt.md
var memoryDreamerPromptTmpl string

// MemoryDreamerParams bundles the dependencies the memory dreamer needs.
type MemoryDreamerParams struct {
	LLM         *llm.Client
	Store       memory.Store
	EmbedClient *embed.Client
	Cfg         *config.Config
	EventBus    *tui.Bus
}

// MemoryDreamerResult summarizes what the dream consolidation cycle did.
type MemoryDreamerResult struct {
	Rewrites int
	Merges   int
	Expires  int
	Creates  int
	Cost     float64
	Error    error
}

// RunMemoryDreamer reviews all memory cards, checks recent changes, and
// runs a tool-calling agent to rewrite/merge/expire as needed.
//
// Returns a result summary. Designed to be called from runDream() before
// NightlyReflect — errors are logged but never fatal.
func RunMemoryDreamer(params MemoryDreamerParams) MemoryDreamerResult {
	var result MemoryDreamerResult

	if params.LLM == nil {
		result.Error = fmt.Errorf("no LLM client configured")
		return result
	}

	log.Info("─── memory dreamer ───")

	// Load all cards.
	cards, err := params.Store.AllCards()
	if err != nil {
		result.Error = fmt.Errorf("loading cards: %w", err)
		return result
	}

	if len(cards) < 2 {
		log.Info("memory dreamer: too few cards to review", "count", len(cards))
		return result
	}

	// Load recent log entries (last 48 hours).
	logEntries, err := params.Store.RecentLogEntries(48)
	if err != nil {
		log.Warn("memory dreamer: failed to load log entries", "err", err)
		// Non-fatal — we can still review cards without the log.
	}

	// Build the set of card IDs that changed since the last dream.
	// A card "changed" if it has memory_log entries OR child memories
	// created in the last 48 hours.
	changedCardIDs := make(map[int64]bool)
	for _, e := range logEntries {
		changedCardIDs[e.CardID] = true
	}

	cutoff := time.Now().Add(-48 * time.Hour)

	// Load child memories for each card so the dreamer can see what's inside.
	// Only include cards that changed or have recent children.
	childrenByCard := make(map[int64][]memory.Memory)
	totalMemories := 0
	var changedCards []memory.MemoryCard
	for _, c := range cards {
		children, err := params.Store.MemoriesByCard(c.ID)
		if err != nil {
			log.Warn("memory dreamer: failed to load children", "card", c.TopicSlug, "err", err)
			continue
		}
		if len(children) == 0 {
			continue
		}

		// Check if any child memory was created recently.
		if !changedCardIDs[c.ID] {
			hasRecent := false
			for _, m := range children {
				if m.Timestamp.After(cutoff) {
					hasRecent = true
					break
				}
			}
			if !hasRecent {
				continue
			}
		}

		childrenByCard[c.ID] = children
		totalMemories += len(children)
		changedCards = append(changedCards, c)
	}

	if len(changedCards) == 0 {
		log.Info("memory dreamer: no cards changed since last dream — skipping")
		return result
	}

	// Build the transcript with only changed cards.
	transcript := buildDreamerTranscript(changedCards, childrenByCard, logEntries)
	log.Infof("memory dreamer: %d/%d cards changed, %d memories, %d recent log entries",
		len(changedCards), len(cards), totalMemories, len(logEntries))

	// Expand prompt template with bot/user names.
	promptContent := params.Cfg.ExpandPrompt(memoryDreamerPromptTmpl)

	// Build tools.Context.
	dryRun := params.Cfg.Dream.DryRun
	tctx := &tools.Context{
		AgentName:           "dream",
		Store:               params.Store,
		EmbedClient:         params.EmbedClient,
		SimilarityThreshold: params.Cfg.Embed.SimilarityThreshold,
		Cfg:                 params.Cfg,
	}

	// Operation counter for safety cap.
	maxOps := params.Cfg.Dream.MaxOperations
	if maxOps == 0 {
		maxOps = 20
	}
	opCount := 0

	// isMutation returns true for tools that modify state (not read-only or control flow).
	isMutation := func(name string) bool {
		return name != "think" && name != "read_card" && name != "list_cards" && name != "done"
	}

	// Run the tool-calling loop via the shared engine.
	loopResult, err := engine.RunLoop(engine.EngineConfig{
		Name:       "dreamer",
		MetricRole: memory.RoleDream,
		LLM:        params.LLM,
		Store:      params.Store,
		ToolDefs:   tools.ToolDefsForAgent("dream", params.Cfg),
		ToolCtx:    tctx,
		Messages: []llm.ChatMessage{
			{Role: "system", Content: promptContent},
			{Role: "user", Content: transcript},
		},
		IterationsPerWindow: params.Cfg.DreamAgent.IterationsPerWindow,
		MaxContinuations:    params.Cfg.DreamAgent.MaxContinuations,
		EventBus:            params.EventBus,

		// Custom continuation message includes the operation count.
		ContinuationMsg: func(window, maxWindows int, summary string) string {
			return fmt.Sprintf(
				"Continuation window %d of %d. You've performed %d operations so far. "+
					"Continue reviewing remaining cards and call done when finished.",
				window, maxWindows, opCount,
			)
		},

		// PreTool: dry-run interception + maxOps safety cap.
		PreTool: func(tc llm.ToolCall, tctx *tools.Context) (string, bool) {
			if !isMutation(tc.Function.Name) {
				return "", false
			}
			if opCount >= maxOps {
				log.Warn("memory dreamer: max operations reached", "max", maxOps)
				return fmt.Sprintf("error: max operations reached (%d). Call done to finish.", maxOps), true
			}
			if dryRun {
				return fmt.Sprintf("[DRY RUN] would execute %s with args: %s",
					tc.Function.Name, engine.TruncateLog(tc.Function.Arguments, 200)), true
			}
			return "", false
		},

		// PostTool: operation counting + audit logging.
		PostTool: func(tc llm.ToolCall, toolResult string, isError bool) {
			if !isMutation(tc.Function.Name) {
				return
			}
			opCount++
			switch tc.Function.Name {
			case "update_card":
				result.Rewrites++
				logDreamerAudit(params.Store, "rewrite", tc.Function.Arguments, toolResult, dryRun)
			case "create_card":
				result.Creates++
				logDreamerAudit(params.Store, "create", tc.Function.Arguments, toolResult, dryRun)
			case "remove_memory":
				result.Expires++
				logDreamerAudit(params.Store, "expire_memory", tc.Function.Arguments, toolResult, dryRun)
			case "merge_memories":
				result.Merges++
				logDreamerAudit(params.Store, "merge_memory", tc.Function.Arguments, toolResult, dryRun)
			}
		},
	})
	if err != nil {
		result.Error = err
		return result
	}

	result.Cost = loopResult.TotalCost

	label := "memory dreamer"
	if dryRun {
		label = "memory dreamer [DRY RUN]"
	}
	log.Infof("%s: %d rewrites, %d merges, %d expires, %d creates | $%.6f",
		label, result.Rewrites, result.Merges, result.Expires, result.Creates, result.Cost)

	return result
}

// buildDreamerTranscript formats all cards with their child memories
// and recent log entries into a structured transcript for the dreamer.
func buildDreamerTranscript(cards []memory.MemoryCard, childrenByCard map[int64][]memory.Memory, logEntries []memory.MemoryLogEntry) string {
	var b strings.Builder

	b.WriteString("# Memory Card Review (changed cards only)\n\n")
	b.WriteString("These cards have new or modified memories since the last dream.\n")
	b.WriteString("Unchanged cards are omitted — they don't need attention.\n\n")

	// Build a map of card IDs to slugs for the log section.
	cardSlugByID := make(map[int64]string)
	for _, c := range cards {
		cardSlugByID[c.ID] = c.TopicSlug
	}

	// Cards grouped by subject, each with their children.
	// Empty cards are omitted — there's nothing for the dreamer to
	// review, and including them just wastes tokens on summary rewrites
	// to "no current X documented" every cycle.
	b.WriteString("## User Cards\n\n")
	for _, c := range cards {
		if c.Subject != "user" {
			continue
		}
		if len(childrenByCard[c.ID]) == 0 {
			continue
		}
		writeCardEntry(&b, c, childrenByCard[c.ID])
	}

	b.WriteString("## Self Cards\n\n")
	for _, c := range cards {
		if c.Subject != "self" {
			continue
		}
		if len(childrenByCard[c.ID]) == 0 {
			continue
		}
		writeCardEntry(&b, c, childrenByCard[c.ID])
	}

	// Recent changelog.
	if len(logEntries) > 0 {
		b.WriteString("## Recent Changes (last 48h)\n\n")
		for _, e := range logEntries {
			slug := cardSlugByID[e.CardID]
			if slug == "" {
				slug = fmt.Sprintf("card#%d", e.CardID)
			}
			fmt.Fprintf(&b, "- [%s] %s → %s: %s\n",
				e.CreatedAt.Format("Jan 02 15:04"), slug, e.Operation, e.Delta)
		}
		b.WriteString("\n")
	}

	return b.String()
}

func writeCardEntry(b *strings.Builder, c memory.MemoryCard, children []memory.Memory) {
	protectedLabel := ""
	if c.Protected {
		protectedLabel = ", PROTECTED"
	}
	age := time.Since(c.UpdatedAt).Hours() / 24
	summary := c.Summary
	if summary == "" {
		summary = "(no summary yet)"
	}
	fmt.Fprintf(b, "### [%s] %s (v%d, %d memories, updated %.0fd ago%s)\nSummary: %s\n",
		c.TopicSlug, c.Name, c.Version, len(children), age, protectedLabel, summary)

	if len(children) == 0 {
		b.WriteString("(empty)\n\n")
		return
	}
	for _, m := range children {
		fmt.Fprintf(b, "- [#%d] %s\n", m.ID, m.Content)
	}
	b.WriteString("\n")
}

// logDreamerAudit writes an audit entry for dreamer operations.
func logDreamerAudit(store memory.Store, op, argsJSON, result string, dryRun bool) {
	if store == nil {
		return
	}

	var args struct {
		TopicSlug string `json:"topic_slug"`
		Delta     string `json:"delta"`
		Reason    string `json:"reason"`
	}
	_ = json.Unmarshal([]byte(argsJSON), &args)

	_ = store.SaveDreamAudit(op, nil, 0, args.TopicSlug, result, args.Delta, dryRun)
}

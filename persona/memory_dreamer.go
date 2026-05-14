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

	"her/config"
	"her/embed"
	"her/llm"
	"her/memory"
	"her/tools"
	"her/tui"

	// Blank imports register the tool handlers the dreamer uses.
	_ "her/tools/create_card"
	_ "her/tools/done"
	_ "her/tools/read_card"
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

	// Compute pairwise similarity hints for organic cards.
	threshold := params.Cfg.Dream.ClusterThreshold
	if threshold == 0 {
		threshold = 0.70
	}
	// Note: similarity hints require card embeddings, which we don't have yet.
	// For now, skip similarity hints. TODO: add card embedding support.

	// Build the transcript.
	transcript := buildCardDreamerTranscript(cards, logEntries)
	log.Infof("memory dreamer: %d cards, %d recent log entries", len(cards), len(logEntries))

	// Expand prompt template with bot/user names.
	promptContent := params.Cfg.ExpandPrompt(memoryDreamerPromptTmpl)

	// Build tools.Context.
	dryRun := params.Cfg.Dream.DryRun
	tctx := &tools.Context{
		Store:               params.Store,
		EmbedClient:         params.EmbedClient,
		SimilarityThreshold: params.Cfg.Embed.SimilarityThreshold,
		Cfg:                 params.Cfg,
	}

	// Load tool definitions — card-based tools for the dreamer.
	dreamerToolDefs := tools.LookupToolDefs(
		[]string{"think", "read_card", "update_card", "create_card", "done"},
		params.Cfg,
	)

	messages := []llm.ChatMessage{
		{Role: "system", Content: promptContent},
		{Role: "user", Content: transcript},
	}

	// Operation counter for safety cap.
	maxOps := params.Cfg.Dream.MaxOperations
	if maxOps == 0 {
		maxOps = 20
	}
	opCount := 0

	// Tool-calling loop — same continuation window pattern as memory agent.
	iterationsPerWindow := params.Cfg.DreamAgent.IterationsPerWindow
	if iterationsPerWindow <= 0 {
		iterationsPerWindow = 15
	}
	maxContinuations := params.Cfg.DreamAgent.MaxContinuations
	if maxContinuations <= 0 {
		maxContinuations = 2
	}

outer:
	for window := 0; window <= maxContinuations; window++ {
		if window > 0 {
			messages = append(messages, llm.ChatMessage{
				Role: "system",
				Content: fmt.Sprintf(
					"Continuation window %d of %d. You've performed %d operations so far. "+
						"Continue reviewing remaining cards and call done when finished.",
					window, maxContinuations, opCount,
				),
			})
			log.Infof("  [dreamer] continuation window %d/%d", window, maxContinuations)
		}

		for i := 0; i < iterationsPerWindow; i++ {
			resp, err := params.LLM.ChatCompletionWithTools(messages, dreamerToolDefs)
			if err != nil {
				log.Error("memory dreamer: LLM error", "err", err)
				result.Error = err
				break outer
			}

			params.Store.SaveMetric(resp.Model, resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens, resp.CostUSD, 0, 0, resp.UsedFallback)
			result.Cost += resp.CostUSD
			log.Infof("  [dreamer] tokens: %d prompt + %d completion | $%.6f | finish=%s",
				resp.PromptTokens, resp.CompletionTokens, resp.CostUSD, resp.FinishReason)

			if len(resp.ToolCalls) == 0 {
				break outer
			}

			messages = append(messages, llm.ChatMessage{
				Role:      "assistant",
				Content:   resp.Content,
				ToolCalls: resp.ToolCalls,
			})

			for _, tc := range resp.ToolCalls {
				// Safety cap — stop executing mutation tools if we hit the limit.
				isMutation := tc.Function.Name != "think" && tc.Function.Name != "read_card" && tc.Function.Name != "done"
				if isMutation && opCount >= maxOps {
					toolResult := fmt.Sprintf("error: max operations reached (%d). Call done to finish.", maxOps)
					messages = append(messages, llm.ChatMessage{
						Role:       "tool",
						Content:    toolResult,
						ToolCallID: tc.ID,
					})
					log.Warn("memory dreamer: max operations reached", "max", maxOps)
					continue
				}

				// Dry-run: intercept mutation tools BEFORE execution.
				var toolResult string
				if dryRun && isMutation {
					toolResult = fmt.Sprintf("[DRY RUN] would execute %s with args: %s",
						tc.Function.Name, truncateLog(tc.Function.Arguments, 200))
				} else {
					toolResult = tools.Execute(tc.Function.Name, tc.Function.Arguments, tctx)
				}
				log.Infof("    [dreamer] %s → %s", tc.Function.Name, truncateLog(toolResult, 150))
				messages = append(messages, llm.ChatMessage{
					Role:       "tool",
					Content:    toolResult,
					ToolCallID: tc.ID,
				})

				if isMutation {
					opCount++
					switch tc.Function.Name {
					case "update_card":
						result.Rewrites++
						logDreamerAudit(params.Store, "rewrite", tc.Function.Arguments, toolResult, dryRun)
					case "create_card":
						result.Creates++
						logDreamerAudit(params.Store, "create", tc.Function.Arguments, toolResult, dryRun)
					}
				}

				// Emit TUI event.
				if params.EventBus != nil {
					params.EventBus.Emit(tui.ToolCallEvent{
						Time:     time.Now(),
						Source:   "dreamer",
						ToolName: tc.Function.Name,
						Args:     truncateLog(tc.Function.Arguments, 200),
						Result:   truncateLog(toolResult, 200),
						IsError:  strings.HasPrefix(toolResult, "error:"),
					})
				}
			}

			if tctx.DoneCalled {
				break outer
			}
		}

		if window == maxContinuations {
			log.Warn("[dreamer] hit max continuations without done signal")
			break outer
		}
	}

	if dryRun {
		log.Infof("memory dreamer [DRY RUN]: %d rewrites, %d merges, %d expires, %d creates | $%.6f",
			result.Rewrites, result.Merges, result.Expires, result.Creates, result.Cost)
	} else {
		log.Infof("memory dreamer: %d rewrites, %d merges, %d expires, %d creates | $%.6f",
			result.Rewrites, result.Merges, result.Expires, result.Creates, result.Cost)
	}

	return result
}

// buildCardDreamerTranscript formats all cards and recent log entries into
// a structured transcript for the memory dreamer agent.
func buildCardDreamerTranscript(cards []memory.MemoryCard, logEntries []memory.MemoryLogEntry) string {
	var b strings.Builder

	b.WriteString("# Memory Card Review\n\n")
	b.WriteString("Review each card for quality, density, and accuracy.\n\n")

	// Build a map of card IDs to slugs for the log section.
	cardSlugByID := make(map[int64]string)
	for _, c := range cards {
		cardSlugByID[c.ID] = c.TopicSlug
	}

	// Cards grouped by subject.
	b.WriteString("## User Cards\n\n")
	for _, c := range cards {
		if c.Subject != "user" {
			continue
		}
		writeCardEntry(&b, c)
	}

	b.WriteString("## Self Cards\n\n")
	for _, c := range cards {
		if c.Subject != "self" {
			continue
		}
		writeCardEntry(&b, c)
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

func writeCardEntry(b *strings.Builder, c memory.MemoryCard) {
	protectedLabel := ""
	if c.Protected {
		protectedLabel = ", PROTECTED"
	}
	age := time.Since(c.UpdatedAt).Hours() / 24
	summary := c.Summary
	if summary == "" {
		summary = "(no summary yet)"
	}
	fmt.Fprintf(b, "### [%s] %s (v%d, updated %.0fd ago%s)\nSummary: %s\n\n",
		c.TopicSlug, c.Name, c.Version, age, protectedLabel, summary)
}

func truncateLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
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

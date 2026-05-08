// Package persona — memory_dreamer.go runs autonomous memory consolidation
// as Step 0 of the nightly dream cycle.
//
// The memory dreamer is a tool-calling agent (same pattern as the memory agent
// in agent/memory_agent.go) that reviews all active memories, clusters them
// by embedding similarity, and decides which to merge, expire, or promote.
//
// It runs BEFORE persona reflection so the reflection sees clean, consolidated
// data rather than duplicates and stale mood snapshots.
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
	_ "her/tools/done"
	_ "her/tools/merge_memories"
	_ "her/tools/recall_memories"
	_ "her/tools/remove_memory"
	_ "her/tools/split_memory"
	_ "her/tools/think"
	_ "her/tools/update_memory"
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
	Merges   int
	Expires  int
	Promotes int
	Cost     float64
	Error    error
}

// RunMemoryDreamer reviews all active memories, clusters them by embedding
// similarity, and runs a tool-calling agent to merge/expire/promote as needed.
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

	// Load all active memories.
	allMemories, err := params.Store.AllActiveMemories()
	if err != nil {
		result.Error = fmt.Errorf("loading memories: %w", err)
		return result
	}

	if len(allMemories) < 3 {
		log.Info("memory dreamer: too few memories to consolidate", "count", len(allMemories))
		return result
	}

	// Cluster by embedding similarity.
	threshold := params.Cfg.Dream.ClusterThreshold
	if threshold == 0 {
		threshold = 0.70
	}
	clusters, lonely := ClusterMemories(allMemories, threshold)
	log.Infof("memory dreamer: %d memories → %d clusters + %d lonely", len(allMemories), len(clusters), len(lonely))

	// If nothing to review, skip the LLM call.
	if len(clusters) == 0 && len(lonely) == 0 {
		log.Info("memory dreamer: nothing to review")
		return result
	}

	// Build the transcript the model will review.
	transcript := buildDreamerTranscript(clusters, lonely)

	// Expand prompt template with bot/user names.
	promptContent := params.Cfg.ExpandPrompt(memoryDreamerPromptTmpl)

	// Build tools.Context — minimal, like the memory agent's.
	dryRun := params.Cfg.Dream.DryRun
	tctx := &tools.Context{
		Store:               params.Store,
		EmbedClient:         params.EmbedClient,
		SimilarityThreshold: params.Cfg.Embed.SimilarityThreshold,
		Cfg:                 params.Cfg,
	}

	// Load tool definitions.
	dreamerToolDefs := tools.LookupToolDefs(
		[]string{"think", "recall_memories", "update_memory", "remove_memory", "split_memory", "merge_memories", "done"},
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
	// Read from config with sensible defaults.
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
						"Continue reviewing remaining clusters/memories and call done when finished.",
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
				isMutation := tc.Function.Name != "think" && tc.Function.Name != "recall_memories" && tc.Function.Name != "done"
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

				// Dry-run: intercept mutation tools BEFORE execution so the DB
				// is never touched. merge_memories handles dry-run internally,
				// but remove_memory and update_memory don't know about it.
				var toolResult string
				if dryRun && isMutation && tc.Function.Name != "merge_memories" {
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
					case "merge_memories":
						result.Merges++
						// merge_memories handler writes its own audit entry
					case "remove_memory":
						result.Expires++
						logDreamerAudit(params.Store, "expire", tc.Function.Arguments, toolResult, dryRun)
					case "update_memory":
						result.Promotes++
						logDreamerAudit(params.Store, "promote", tc.Function.Arguments, toolResult, dryRun)
					case "split_memory":
						logDreamerAudit(params.Store, "split", tc.Function.Arguments, toolResult, dryRun)
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
		log.Infof("memory dreamer [DRY RUN]: %d merges, %d expires, %d promotes | $%.6f",
			result.Merges, result.Expires, result.Promotes, result.Cost)
	} else {
		log.Infof("memory dreamer: %d merges, %d expires, %d promotes | $%.6f",
			result.Merges, result.Expires, result.Promotes, result.Cost)
	}

	return result
}

// buildDreamerTranscript formats clusters and lonely memories into a structured
// transcript for the memory dreamer agent.
func buildDreamerTranscript(clusters []MemoryCluster, lonely []memory.Memory) string {
	var b strings.Builder

	b.WriteString("# Memory Consolidation Review\n\n")
	b.WriteString("Review each cluster for merge opportunities and each lonely memory for staleness.\n\n")

	// Clusters — potential merge candidates.
	for i, c := range clusters {
		fmt.Fprintf(&b, "## Cluster %d (%d memories)\n", i+1, len(c.Memories))
		for _, m := range c.Memories {
			age := time.Since(m.Timestamp).Hours() / 24
			fmt.Fprintf(&b, "- [ID=%d, cat=%s, imp=%d, subj=%s, age=%.0fd] %s\n",
				m.ID, m.Category, m.Importance, m.Subject, age, m.Content)
		}
		b.WriteString("\n")
	}

	// Lonely memories — staleness review.
	if len(lonely) > 0 {
		b.WriteString("## Lonely memories (staleness review)\n")
		b.WriteString("These don't cluster with anything. Check if they're stale moods, past events, or still relevant.\n\n")
		for _, m := range lonely {
			age := time.Since(m.Timestamp).Hours() / 24
			fmt.Fprintf(&b, "- [ID=%d, cat=%s, imp=%d, subj=%s, age=%.0fd] %s\n",
				m.ID, m.Category, m.Importance, m.Subject, age, m.Content)
		}
	}

	return b.String()
}

func truncateLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// logDreamerAudit writes an audit entry for expire/promote/split operations.
// merge_memories writes its own audit entry; this covers tools that don't
// know they're being called by the dreamer.
func logDreamerAudit(store memory.Store, op, argsJSON, result string, dryRun bool) {
	if store == nil {
		return
	}

	var args struct {
		MemoryID  int64   `json:"memory_id"`
		MemoryIDs []int64 `json:"memory_ids"`
		Reason    string  `json:"reason"`
		Content   string  `json:"content"`
	}
	_ = json.Unmarshal([]byte(argsJSON), &args)

	var sourceIDs []int64
	if len(args.MemoryIDs) > 0 {
		sourceIDs = args.MemoryIDs
	} else if args.MemoryID > 0 {
		sourceIDs = []int64{args.MemoryID}
	}

	_ = store.SaveDreamAudit(op, sourceIDs, 0, "", result, args.Reason, dryRun)
}

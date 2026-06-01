// Package bot — fast_path.go implements the fast-path classifier and direct
// reply path. When enabled, a cheap LLM call (Gemini Flash Lite) decides
// whether each message needs the full driver agent pipeline or can go
// straight to the chat model.
//
// The idea: most companion chat messages ("haha", "goodnight", "how are you")
// don't need tool calls, web search, or multi-step agent reasoning. Routing
// them directly to the chat model saves 3-5 driver iterations per turn —
// roughly 50% of the per-turn cost.
//
// The fast path still assembles the full chat context (persona, mood, memories,
// conversation history) via the layer registry. It just skips the driver agent's
// orchestration loop. Quality stays high because the layers do the heavy lifting.
package bot

import (
	_ "embed"
	"fmt"
	"strings"
	"time"

	"her/layers"
	"her/llm"
	"her/memory"
	"her/scrub"
	"her/tools"
	"her/tui"
	"her/turn"
)

// fastPathSystemPrompt is loaded from fast_path_prompt.md — the routing
// classifier prompt for deciding SKIP (direct chat) vs PASS (full pipeline).
// Embedded at compile time so no file I/O at runtime.
//
//go:embed fast_path_prompt.md
var fastPathSystemPrompt string

// shouldFastPath checks preconditions for the fast-path classifier.
// Returns false if the fast path can't or shouldn't be used for this turn.
func (b *Bot) shouldFastPath(input AgentInput) bool {
	if !b.cfg.Driver.FastPath {
		return false
	}
	if b.classifierLLM == nil {
		return false
	}
	// Images always need the driver (view_image tool).
	if input.ImageBase64 != "" {
		return false
	}
	// First message in a conversation should go through the full pipeline
	// so the agent can do proper greeting/context-setting behavior.
	count, err := b.store.MessageCountSince(input.ConversationID, 0)
	if err != nil || count <= 1 {
		return false
	}
	return true
}

// classifyRoute calls the classifier LLM to decide SKIP or PASS.
// Returns "SKIP" for fast path, "PASS" for full pipeline.
// Fails open to "PASS" on any error (same pattern as the memory classifier).
func (b *Bot) classifyRoute(scrubbedText, conversationID string) string {
	// Build a short context snippet from recent messages so the classifier
	// can see whether this is mid-conversation (more likely SKIP) or a
	// cold start (more likely PASS).
	snippet := ""
	recent, err := b.store.RecentMessages(conversationID, 3)
	if err == nil && len(recent) > 0 {
		var lines []string
		for _, msg := range recent {
			text := msg.ContentScrubbed
			if text == "" {
				text = msg.ContentRaw
			}
			if len(text) > 150 {
				text = text[:150] + "..."
			}
			lines = append(lines, fmt.Sprintf("%s: %s", msg.Role, text))
		}
		snippet = strings.Join(lines, "\n")
	}

	userPrompt := fmt.Sprintf("Recent conversation:\n%s\n\nNew message to route:\n%s", snippet, scrubbedText)

	messages := []llm.ChatMessage{
		{Role: "system", Content: fastPathSystemPrompt},
		{Role: "user", Content: userPrompt},
	}

	resp, err := b.classifierLLM.ChatCompletion(messages)
	if err != nil {
		// Fail-open: if the classifier is down, use the full pipeline.
		log.Warn("fast-path: classifier error, falling back to PASS", "err", err)
		return "PASS"
	}

	// Save metrics for observability.
	if b.store != nil {
		b.store.SaveMetric(resp.Model, resp.PromptTokens, resp.CompletionTokens,
			resp.TotalTokens, resp.CostUSD, 0, 0, false, memory.RoleClassifier)
	}

	verdict := strings.TrimSpace(strings.ToUpper(resp.Content))
	if strings.HasPrefix(verdict, "SKIP") {
		log.Infof("fast-path: SKIP — routing directly to chat model ($%.6f)", resp.CostUSD)
		return "SKIP"
	}
	log.Infof("fast-path: PASS — using full driver pipeline ($%.6f)", resp.CostUSD)
	return "PASS"
}

// runFastPath handles a turn without the driver agent. It assembles the
// chat context via the layer registry (same layers the reply tool uses),
// does a quick semantic search for relevant memories, calls the chat model,
// and delivers the reply.
//
// This is essentially what the reply tool does, minus:
//   - Agent instruction (no driver ran, so no planning guidance)
//   - Search context (no web search happened)
//   - Style/safety gates (skipped for speed — fast-path turns are simple
//     enough that degenerate outputs are rare)
//
// The reply tool is ~600 lines because it handles retries, gates, pagination,
// and edge cases. The fast path is intentionally simpler — it trades those
// safeguards for speed on turns that don't need them.
func (b *Bot) runFastPath(
	fe Frontend,
	input AgentInput,
	scrubbedText string,
	vault *scrub.Vault,
	tracker *turn.Tracker,
	statusCallback tools.StatusCallback,
	streamCallback tools.StreamCallback,
	ttsCallback tools.TTSCallback,
	eventBus *tui.Bus,
	triggerMsgID int64,
) error {
	start := time.Now()

	// Emit a trace event so the Gradio/WebSocket trace panel shows
	// that this turn used the fast path instead of the full pipeline.
	if eventBus != nil {
		eventBus.Emit(tui.ToolCallEvent{
			Time:     time.Now(),
			TurnID:   triggerMsgID,
			ToolName: "fast-path",
			Args:     "SKIP",
			Result:   "routing directly to chat model, skipping driver agent",
		})
	}

	// --- Auto-recall memories via semantic search ---
	// In the full pipeline, the driver agent calls recall_memories and
	// chooses which facts to pass to the chat model. Here we do it
	// automatically: embed the user's message and grab the top matches.
	// This is the one place the fast path "breaks" the agent-as-gatekeeper
	// rule — and it's fine because simple conversational turns need basic
	// context, not curated recall.
	var autoMemories []string
	if b.embedClient != nil {
		vec, err := b.embedClient.Embed(scrubbedText)
		if err == nil {
			topK := b.cfg.Memory.MaxFactsInContext
			if topK <= 0 {
				topK = 5
			}
			memories, err := b.store.SemanticSearch(vec, topK)
			if err == nil {
				for _, m := range memories {
					if m.Distance <= b.cfg.Embed.SimilarityThreshold {
						autoMemories = append(autoMemories, m.Content)
					}
				}
				// Mark as recalled for usage tracking (same as the reply tool).
				if len(memories) > 0 {
					ids := make([]int64, 0, len(memories))
					for _, m := range memories {
						if m.Distance <= b.cfg.Embed.SimilarityThreshold {
							ids = append(ids, m.ID)
						}
					}
					if len(ids) > 0 {
						_ = b.store.MarkMemoriesRecalled(ids)
					}
				}
			}
		}
	}

	// --- Build chat context via the layer registry ---
	// Load the latest conversation summary (from a previous compaction)
	// instead of running compaction ourselves — saves an LLM call.
	summary, _, err := b.store.LatestSummary(input.ConversationID, "chat")
	if err != nil {
		log.Warn("fast-path: loading summary", "err", err)
	}

	chatLayerCtx := &layers.LayerContext{
		Store:               b.store,
		Cfg:                 b.cfg,
		EmbedClient:         b.embedClient,
		AgentPassedMemories: autoMemories,
		ConversationSummary: summary,
		ConversationID:      input.ConversationID,
		ScrubbedUserMessage: scrubbedText,
	}
	systemPrompt, chatLayerResults := layers.BuildAll(layers.StreamChat, chatLayerCtx)

	// Log layer shape for observability.
	var chatTotalTokens int
	for _, lr := range chatLayerResults {
		chatTotalTokens += lr.Tokens
		if lr.Detail != "" {
			log.Infof("  [fast-path chat layer] %s: ~%d tokens (%s)", lr.Name, lr.Tokens, lr.Detail)
		} else {
			log.Infof("  [fast-path chat layer] %s: ~%d tokens", lr.Name, lr.Tokens)
		}
	}
	log.Infof("  fast-path system prompt total: ~%d tokens", chatTotalTokens)

	// --- Build message list ---
	var llmMessages []llm.ChatMessage
	llmMessages = append(llmMessages, llm.ChatMessage{
		Role:    "system",
		Content: systemPrompt,
	})

	// Recent conversation history — same logic as the reply tool.
	recentMsgs, err := b.store.RecentMessages(input.ConversationID, b.cfg.Memory.RecentMessages)
	if err != nil {
		log.Error("fast-path: loading history", "err", err)
	} else {
		var prevDay time.Time
		for _, msg := range recentMsgs {
			msgDate := time.Date(msg.Timestamp.Year(), msg.Timestamp.Month(),
				msg.Timestamp.Day(), 0, 0, 0, 0, msg.Timestamp.Location())
			if !prevDay.IsZero() && !msgDate.Equal(prevDay) {
				llmMessages = append(llmMessages, llm.ChatMessage{
					Role:    "system",
					Content: "--- the above messages are from a previous day ---",
				})
			}
			prevDay = msgDate

			content := msg.ContentScrubbed
			if content == "" {
				content = msg.ContentRaw
			}
			llmMessages = append(llmMessages, llm.ChatMessage{
				Role:    msg.Role,
				Content: content,
			})
		}
	}

	// Length directive: fast-path turns are conversational, keep replies brief.
	llmMessages = append(llmMessages, llm.ChatMessage{
		Role:    "system",
		Content: "Length: Keep this SHORT. One sentence, maybe a few words. Fragments are fine. Don't elaborate unless asked.",
	})
	llmMessages = append(llmMessages, llm.ChatMessage{
		Role:    "user",
		Content: scrubbedText,
	})

	// --- Call the chat model ---
	var resp *llm.ChatResponse
	if streamCallback != nil {
		resp, err = b.llm.ChatCompletionStreaming(llmMessages, func(token string) {
			_ = streamCallback(token)
		})
	} else {
		resp, err = b.llm.ChatCompletion(llmMessages)
	}
	latencyMs := int(time.Since(start).Milliseconds())

	if err != nil {
		log.Error("fast-path: chat LLM error", "err", err)
		return fmt.Errorf("fast-path chat error: %w", err)
	}

	log.Infof("  fast-path reply: %d prompt + %d completion = %d total | $%.6f | %dms",
		resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens, resp.CostUSD, latencyMs)

	// Emit reply event for TUI/traces.
	if eventBus != nil {
		eventBus.Emit(tui.ReplyEvent{
			Time:             time.Now(),
			TurnID:           triggerMsgID,
			Text:             truncate(resp.Content, 200),
			PromptTokens:     resp.PromptTokens,
			CompletionTokens: resp.CompletionTokens,
			TotalTokens:      resp.TotalTokens,
			CostUSD:          resp.CostUSD,
			LatencyMs:        latencyMs,
		})
	}

	// Deanonymize PII tokens before sending to the user.
	replyText := scrub.Deanonymize(resp.Content, vault)

	// --- Deliver ---
	if statusCallback != nil {
		if err := statusCallback(replyText); err != nil {
			log.Error("fast-path: send failed", "err", err)
			return fmt.Errorf("fast-path send error: %w", err)
		}
	}

	// Save to DB after confirmed delivery.
	respID, err := b.store.SaveMessage("assistant", resp.Content, resp.Content, input.ConversationID)
	if err != nil {
		log.Error("fast-path: saving response", "err", err)
	}
	if respID > 0 {
		b.store.UpdateMessageTokenCount(respID, resp.CompletionTokens)
		b.store.SaveMetric(resp.Model, resp.PromptTokens, resp.CompletionTokens,
			resp.TotalTokens, resp.CostUSD, latencyMs, respID, resp.UsedFallback, memory.RoleChat)
	}

	// TTS for voice replies.
	if ttsCallback != nil {
		go ttsCallback(replyText)
	}

	log.Infof("  fast-path complete in %dms", latencyMs)
	return nil
}

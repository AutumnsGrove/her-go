// Package bot — fast_path.go implements the fast-path classifier and direct
// reply path. When enabled, a cheap LLM call (classifier model) decides
// whether each message needs the full driver agent pipeline or can go
// straight to the chat model.
//
// Most companion chat messages ("haha", "goodnight", "how are you") don't
// need tool calls, web search, or multi-step agent reasoning. Routing them
// directly to the chat model saves 3-5 driver iterations per turn.
//
// The fast path still assembles the full chat context (persona, mood,
// memories, conversation history) via the layer registry. It just skips
// the driver agent's orchestration loop.
package bot

import (
	"fmt"
	"os"
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

// fastPathPromptFile is the path to the routing classifier prompt.
// Hot-loaded on every call so you can tune routing without restarting.
const fastPathPromptFile = "fast_path_prompt.md"

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
// Fails open to "PASS" on any error — same pattern as the memory classifier.
func (b *Bot) classifyRoute(scrubbedText, conversationID string) string {
	// Hot-load the routing prompt so it can be tuned without restarting.
	promptBytes, err := os.ReadFile(fastPathPromptFile)
	if err != nil {
		log.Warn("fast-path: can't read prompt file, falling back to PASS", "err", err)
		return "PASS"
	}

	// Build a short context snippet from recent messages so the classifier
	// can see whether this is mid-conversation or a cold start.
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
		{Role: "system", Content: string(promptBytes)},
		{Role: "user", Content: userPrompt},
	}

	resp, err := b.classifierLLM.ChatCompletion(messages)
	if err != nil {
		log.Warn("fast-path: classifier error, falling back to PASS", "err", err)
		return "PASS"
	}

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

// fastPathResult holds the output of a fast-path turn so the caller can
// feed it into the substance gate + batching system identically to a
// normal driver turn.
type fastPathResult struct {
	replyText string
	costUSD   float64
}

// runFastPath handles a turn without the driver agent. It assembles the
// chat context via the layer registry (same layers the reply tool uses),
// does a quick semantic search for relevant memories, calls the chat model,
// and delivers the reply.
//
// Returns the result so the caller can feed it into the existing substance
// gate and background agent batching — fast-path turns are batched
// identically to normal turns.
func (b *Bot) runFastPath(
	fe Frontend,
	input AgentInput,
	scrubbedText string,
	vault *scrub.Vault,
	tracker *turn.Tracker,
	statusCallback func(string) error,
	streamCallback tools.StreamCallback,
	onMessageSend tools.MessageSendCallback,
) (*fastPathResult, error) {
	start := time.Now()

	// Emit routing decision through the event bus so all subscribers
	// (sim adapter, TUI, Telegram traces, file logger) see it.
	if b.eventBus != nil {
		b.eventBus.Emit(tui.ToolCallEvent{
			Time:     time.Now(),
			TurnID:   input.TriggerMsgID,
			Source:   "main",
			ToolName: "fast-path",
			Args:     "SKIP",
			Result:   "routing directly to chat model, skipping driver agent",
		})
	}

	// --- Auto-recall memories via semantic search ---
	// In the full pipeline, the driver agent calls recall_memories.
	// Here we do it automatically: embed the user's message and grab
	// the top matches. Simple conversational turns need basic context,
	// not curated recall.
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

	// --- Build chat context via the layer registry ---
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

	var chatTotalTokens int
	for _, lr := range chatLayerResults {
		chatTotalTokens += lr.Tokens
	}
	log.Infof("  fast-path system prompt: ~%d tokens (%d layers)", chatTotalTokens, len(chatLayerResults))

	// --- Build message list ---
	var llmMessages []llm.ChatMessage
	llmMessages = append(llmMessages, llm.ChatMessage{
		Role:    "system",
		Content: systemPrompt,
	})

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

	// Fast-path turns are conversational — keep replies brief.
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
		return nil, fmt.Errorf("fast-path chat error: %w", err)
	}

	log.Infof("  fast-path reply: %d prompt + %d completion = %d total | $%.6f | %dms",
		resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens, resp.CostUSD, latencyMs)

	// Emit reply event through the event bus.
	if b.eventBus != nil {
		b.eventBus.Emit(tui.ReplyEvent{
			Time:             time.Now(),
			TurnID:           input.TriggerMsgID,
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
			return nil, fmt.Errorf("fast-path send error: %w", err)
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

	if onMessageSend != nil {
		go onMessageSend(tools.MessageSendInfo{
			Text:         replyText,
			IsFirstReply: true,
			Model:        resp.Model,
			UsedFallback: resp.UsedFallback,
			CostUSD:      resp.CostUSD,
		})
	}

	log.Infof("  fast-path complete in %dms", latencyMs)
	return &fastPathResult{
		replyText: resp.Content,
		costUSD:   resp.CostUSD,
	}, nil
}

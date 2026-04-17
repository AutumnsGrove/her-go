// Package reply implements the reply tool — generates and delivers the
// user-facing response via the chat model (Deepseek V3.2).
//
// This is the most complex tool in the system. It:
//   1. Builds the chat system prompt from the layer registry (persona,
//      memory, time, etc.)
//   2. Assembles the conversation history with day-boundary detection
//   3. Calls the chat LLM with the agent's instruction and context
//   4. Guards against degenerate and overly-long responses
//   5. Saves the response to the DB and delivers it to Telegram + TTS
//
// Previously execReply lived inside agent/agent.go as a special-cased
// function. Moving it here makes reply a first-class tool: traceable,
// testable, and dispatched uniformly alongside all other tools.
package reply

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"her/layers"
	"her/llm"
	"her/logger"
	"her/scrub"
	"her/tools"
	"her/tui"
)

var log = logger.WithPrefix("tools/reply")

func init() {
	tools.Register("reply", Handle)
}

// Handle generates and delivers a response to the user. It builds the chat
// system prompt via the layer registry, calls the chat LLM, and handles
// delivery to Telegram and TTS. Returns a preview of the reply so the
// agent knows what was said.
func Handle(argsJSON string, ctx *tools.Context) string {
	// Reset fallback tracking from any previous reply call in this turn.
	// Without this, a fallback on reply #1 would incorrectly flag reply #2.
	ctx.ReplyUsedFallback = false
	ctx.ReplyModel = ""

	var args struct {
		Instruction string   `json:"instruction"`
		Context     string   `json:"context"`
		Facts       []string `json:"facts"` // facts retrieved via recall_memories to inject into chat context
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	// If the agent passed facts, store them on ctx so the chat layer can use them.
	// These override the auto-searched RelevantFacts for this reply.
	if len(args.Facts) > 0 {
		ctx.AgentPassedFacts = args.Facts
	}

	// Build the system prompt using the layer registry.
	// Each layer (persona, memory, time, etc.) lives in its own file under
	// layers/ and auto-registers via init(). StreamChat selects only the
	// layers relevant to the conversational model (not the agent planner).
	chatLayerCtx := &layers.LayerContext{
		Store:               ctx.Store,
		Cfg:                 ctx.Cfg,
		EmbedClient:         ctx.EmbedClient,
		RelevantFacts:       ctx.RelevantFacts,
		AgentPassedFacts:    ctx.AgentPassedFacts,
		ConversationSummary: ctx.ConversationSummary,
		ConversationID:      ctx.ConversationID,
		ScrubbedUserMessage: ctx.ScrubbedUserMessage,
		ExpenseContext:      ctx.ExpenseContext,
	}
	systemPrompt, chatLayerResults := layers.BuildAll(layers.StreamChat, chatLayerCtx)

	// Log chat prompt shape for observability.
	var chatTotalTokens int
	for _, lr := range chatLayerResults {
		chatTotalTokens += lr.Tokens
		if lr.Detail != "" {
			log.Infof("  [chat layer] %s: ~%d tokens (%s)", lr.Name, lr.Tokens, lr.Detail)
		} else {
			log.Infof("  [chat layer] %s: ~%d tokens", lr.Name, lr.Tokens)
		}
		// Pass injected facts observability to the TUI.
		if ctx.EventBus != nil {
			for _, f := range lr.InjectedFacts {
				factArgs := fmt.Sprintf("#%d %s", f.ID, f.Source)
				if f.Distance > 0 {
					factArgs = fmt.Sprintf("#%d %s dist=%.2f", f.ID, f.Source, f.Distance)
				}
				ctx.EventBus.Emit(tui.ToolCallEvent{
					Time:     time.Now(),
					TurnID:   ctx.TriggerMsgID,
					ToolName: "fact→chat",
					Args:     factArgs,
					Result:   truncate(f.Fact, 80),
				})
			}
		}
	}
	log.Infof("  chat system prompt total: ~%d tokens", chatTotalTokens)

	// Combine any accumulated search context with the explicit context parameter.
	fullContext := ctx.SearchContext
	if args.Context != "" {
		if fullContext != "" {
			fullContext += "\n\n"
		}
		fullContext += args.Context
	}

	// Build the message list for the conversational model.
	var llmMessages []llm.ChatMessage
	llmMessages = append(llmMessages, llm.ChatMessage{
		Role:    "system",
		Content: systemPrompt,
	})

	// Add conversation history so the model has context of the ongoing chat.
	recentMsgs, err := ctx.Store.RecentMessages(ctx.ConversationID, ctx.Cfg.Memory.RecentMessages)
	if err != nil {
		log.Error("reply: loading history", "err", err)
	} else {
		// prevDay tracks the calendar date of the last message we appended.
		// When consecutive messages cross a midnight boundary, we inject a
		// system message so the chat model knows the earlier context is
		// from a different day (prevents perseveration on stale topics).
		var prevDay time.Time

		for _, msg := range recentMsgs {
			// For continuation replies (2nd, 3rd, etc.), strip out this
			// turn's messages — the trigger message and any replies we
			// already sent. Without this, the model sees its own first
			// reply in history plus the same user message appended below,
			// thinks it already answered, and generates identical output.
			// We keep everything BEFORE this turn so the model still has
			// the broader conversation context.
			if ctx.ReplyCount > 0 && msg.ID >= ctx.TriggerMsgID {
				continue
			}

			// Day boundary detection — inject a separator when messages
			// cross midnight so the model treats earlier context as
			// "yesterday" rather than the active conversation topic.
			msgDate := time.Date(msg.Timestamp.Year(), msg.Timestamp.Month(), msg.Timestamp.Day(), 0, 0, 0, 0, msg.Timestamp.Location())
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

	// Build the user message. Search context and the agent's instruction
	// go into a lightweight system note so they don't masquerade as user
	// speech (which confused some models and caused degenerate outputs).
	if args.Instruction != "" || fullContext != "" {
		var note strings.Builder
		if fullContext != "" {
			note.WriteString("The following reference material may be useful for your response — use it naturally, don't quote verbatim or mention that you searched unless appropriate:\n\n")
			note.WriteString(fullContext)
			note.WriteString("\n\n")
		}
		if args.Instruction != "" {
			note.WriteString("Guidance from the assistant's planning layer: ")
			note.WriteString(args.Instruction)
		}
		llmMessages = append(llmMessages, llm.ChatMessage{
			Role:    "system",
			Content: note.String(),
		})
	}
	llmMessages = append(llmMessages, llm.ChatMessage{
		Role:    "user",
		Content: ctx.ScrubbedUserMessage,
	})

	// Call the conversational model.
	start := time.Now()
	resp, err := ctx.ChatLLM.ChatCompletion(llmMessages)
	latencyMs := int(time.Since(start).Milliseconds())

	if err != nil {
		log.Error("reply: LLM error", "err", err)
		return fmt.Sprintf("error generating response: %v", err)
	}

	ctx.ReplyCost += resp.CostUSD
	ctx.ReplyUsedFallback = resp.UsedFallback
	ctx.ReplyModel = resp.Model
	log.Infof("  reply: %d prompt + %d completion = %d total | $%.6f | %dms",
		resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens, resp.CostUSD, latencyMs)
	if ctx.EventBus != nil {
		ctx.EventBus.Emit(tui.ReplyEvent{
			Time:             time.Now(),
			TurnID:           ctx.TriggerMsgID,
			Text:             truncate(resp.Content, 200),
			PromptTokens:     resp.PromptTokens,
			CompletionTokens: resp.CompletionTokens,
			TotalTokens:      resp.TotalTokens,
			CostUSD:          resp.CostUSD,
			LatencyMs:        latencyMs,
		})
	}

	// Guard against degenerate responses. If the chat model returned
	// something suspiciously short (< 10 chars) or repetitive, it was
	// likely rate-limited or glitching. These garbage responses poison
	// the conversation history if saved, causing a feedback loop where
	// every subsequent turn degenerates further (the "ohohoh" incident).
	if isDegenerate(resp.Content) {
		log.Warn("reply: degenerate response detected, retrying once", "content", truncate(resp.Content, 80))
		// One retry — if the model is genuinely down, the fallback
		// in the agent loop will catch it.
		resp, err = ctx.ChatLLM.ChatCompletion(llmMessages)
		if err != nil {
			log.Error("reply: retry LLM error", "err", err)
			return fmt.Sprintf("error generating response: %v", err)
		}
		if isDegenerate(resp.Content) {
			log.Error("reply: degenerate response on retry too", "content", truncate(resp.Content, 80))
			return "error: model returned a degenerate response. Try again in a moment."
		}
	}

	// Length guard. Telegram rejects messages over 4096 characters with
	// MESSAGE_TOO_LONG, and historically that error was logged then
	// silently swallowed — the user got nothing. Worse, the chat model
	// occasionally generates 16k+ char runaway replies. We catch that
	// here, before any downstream side effects fire (DB save, Telegram
	// send, TTS), and return a rejection string the agent can react to.
	//
	// 3500 leaves margin under Telegram's 4096 limit for deanonymization
	// expansion (PII placeholders → real values may grow the string)
	// and any markdown/emoji byte overhead.
	const maxReplyChars = 3500
	if len(resp.Content) > maxReplyChars {
		log.Warn("reply: response too long, rejecting",
			"chars", len(resp.Content),
			"max", maxReplyChars,
			"preview", truncate(resp.Content, 120))
		return fmt.Sprintf(
			"rejected: response was %d characters (max %d). The reply was NOT delivered to the user. "+
				"Call reply again with an instruction that explicitly demands a SHORT response — "+
				"1-3 sentences, under 500 characters. Do not let the chat model riff or expand.",
			len(resp.Content), maxReplyChars)
	}

	// Style gate — optional soft check for AI writing patterns.
	// Only runs when a classifier is configured (ctx.ClassifyReplyFunc != nil).
	// Retries once with a direct hint if a pattern is detected.
	// Fail-open: if the retry still has issues, we deliver anyway — the style
	// gate should never block a reply from reaching the user.
	if ctx.ClassifyReplyFunc != nil {
		styleVerdict := ctx.ClassifyReplyFunc(resp.Content)
		if !styleVerdict.Allowed {
			hint := styleVerdict.Reason
			if hint == "" {
				hint = "avoid formulaic AI openers or closers"
			}
			log.Info("reply: style gate flagged response, retrying once",
				"verdict", styleVerdict.Type, "reason", hint,
				"preview", truncate(resp.Content, 80))

			// Retry with the hint injected as a final system nudge.
			// We append rather than replace so the original instruction
			// context is still there — just with a correction on top.
			hintMessages := append(llmMessages, llm.ChatMessage{
				Role:    "system",
				Content: "Style note: " + hint + ". Rephrase to be more natural and direct.",
			})
			retryResp, retryErr := ctx.ChatLLM.ChatCompletion(hintMessages)
			if retryErr == nil && !isDegenerate(retryResp.Content) && len(retryResp.Content) <= maxReplyChars {
				resp = retryResp
				ctx.ReplyCost += retryResp.CostUSD
				log.Info("reply: style gate retry accepted", "preview", truncate(resp.Content, 80))
			} else {
				log.Warn("reply: style gate retry failed or invalid, delivering original")
			}
		}
	}

	// Save the response to the database.
	respID, err := ctx.Store.SaveMessage("assistant", resp.Content, resp.Content, ctx.ConversationID)
	if err != nil {
		log.Error("reply: saving response", "err", err)
	}

	if respID > 0 {
		ctx.Store.UpdateMessageTokenCount(respID, resp.CompletionTokens)
		ctx.Store.SaveMetric(resp.Model, resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens, resp.CostUSD, latencyMs, respID)
	}

	// Deanonymize PII tokens before sending to the user.
	// The LLM might have used placeholders like [PHONE_1] in its response —
	// we swap those back to the real values before the user sees it.
	replyText := scrub.Deanonymize(resp.Content, ctx.ScrubVault)

	// Duplicate reply guard — if the agent calls reply twice with the
	// same text, skip the second one. Some models loop think→reply→think→reply
	// with identical content.
	if ctx.ReplyCalled && replyText == ctx.ReplyText {
		log.Warn("reply: duplicate detected, skipping")
		return "reply skipped (duplicate of previous reply)"
	}

	// Deliver the response to Telegram.
	// First reply: edit the placeholder message (statusCallback).
	// Follow-up replies: send as a new message (sendCallback) so both
	// are visible — e.g., "let me look that up" → "here's what I found".
	if ctx.ReplyCalled && ctx.SendCallback != nil {
		// Follow-up reply — send as a new message.
		if err := ctx.SendCallback(replyText); err != nil {
			log.Error("reply: sending follow-up to Telegram", "err", err)
		}
	} else if ctx.StatusCallback != nil {
		// First reply — edit the placeholder.
		if err := ctx.StatusCallback(replyText); err != nil {
			log.Error("reply: sending to Telegram", "err", err)
		}
	}

	// Fire TTS immediately — don't wait for the agent loop to finish.
	// This runs in a goroutine so the agent can keep thinking/acting
	// while the voice memo is being synthesized and sent.
	if ctx.TTSCallback != nil {
		go ctx.TTSCallback(replyText)
	}

	ctx.ReplyCalled = true
	ctx.ReplyCount++
	ctx.ReplyText = replyText

	// Stage reset: send a new Telegram placeholder so that any follow-up
	// work (search status updates, additional replies) doesn't overwrite
	// the reply we just sent. After the reset, statusCallback targets the
	// new placeholder and replyCalled is cleared so the next reply edits
	// it instead of using sendCallback.
	if ctx.StageResetCallback != nil {
		if err := ctx.StageResetCallback(); err != nil {
			log.Warn("reply: stage reset failed", "err", err)
		} else {
			ctx.ReplyCalled = false
		}
	}

	// Feed the actual reply text back to the agent so it knows what was
	// said. Without this, the agent has no visibility into the chat model's
	// output and may call reply again with the same instruction. The
	// truncation keeps the tool result from bloating the context.
	preview := replyText
	if len(preview) > 300 {
		preview = preview[:300] + "..."
	}
	return fmt.Sprintf("reply delivered to user: %q\n\nYour message has been sent. Call done to end your turn unless you have pending work (e.g., a search in progress).", preview)
}

// isDegenerate detects garbage LLM outputs that would poison conversation
// history if saved. Catches short responses, excessive repetition (like
// "ohohohohoh..."), and empty responses. These typically happen when
// the model is rate-limited, overloaded, or in a degenerate loop.
func isDegenerate(text string) bool {
	trimmed := strings.TrimSpace(text)

	// Empty or extremely short — a real reply should be at least a
	// short sentence. Single words like "you", "ok", "hi" indicate
	// the chatLLM choked (rate limit, timeout, degenerate output).
	if len(trimmed) < 10 {
		return true
	}

	// Repetition detector: if any 2-4 character substring repeats to
	// fill most of the response, it's degenerate. We check by taking
	// a small prefix and seeing if repeating it reconstructs the text.
	if len(trimmed) > 20 {
		for patLen := 1; patLen <= 4; patLen++ {
			pat := trimmed[:patLen]
			repeated := strings.Repeat(pat, len(trimmed)/patLen+1)
			// If the repeated pattern matches at least 90% of the text,
			// it's a repetition loop.
			if len(repeated) >= len(trimmed) && repeated[:len(trimmed)] == trimmed {
				return true
			}
		}
	}

	return false
}

// truncate shortens a string for log output, collapsing newlines and
// adding "..." if it was cut. Package-private — only needed internally.
func truncate(s string, maxLen int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

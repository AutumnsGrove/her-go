// Package reply implements the reply tool — generates and delivers the
// user-facing response via the chat model.
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

	"her/classifier"
	"her/layers"
	"her/llm"
	"her/logger"
	"her/memory"
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
	// Hard cap on replies per turn. The agent's internal style/safety gates
	// already handle retries — this prevents the agent model from calling
	// reply in a self-correction loop (think→reply→think→reply with
	// near-identical content). Default 2: enough for "let me look that up"
	// followed by the actual answer.
	maxReplies := ctx.Cfg.Agent.MaxRepliesPerTurn
	if maxReplies <= 0 {
		maxReplies = 2
	}
	if ctx.ReplyCount >= maxReplies {
		log.Warn("reply: max replies reached", "count", ctx.ReplyCount, "max", maxReplies)
		return fmt.Sprintf(
			"rejected: you already sent %d replies this turn (max %d). "+
				"Your earlier replies were delivered successfully. Call done now.",
			ctx.ReplyCount, maxReplies)
	}

	// Reset fallback tracking from any previous reply call in this turn.
	// Without this, a fallback on reply #1 would incorrectly flag reply #2.
	ctx.ReplyUsedFallback = false
	ctx.ReplyModel = ""

	var args struct {
		Instruction string   `json:"instruction"`
		Context     string   `json:"context"`
		Memories    []string `json:"memories"` // memories retrieved via recall_memories to inject into chat context
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	// If the agent passed memories, store them on ctx so the chat layer can use them.
	// These override the auto-searched RelevantMemories for this reply.
	if len(args.Memories) > 0 {
		ctx.AgentPassedMemories = args.Memories
	}

	// Build the system prompt using the layer registry.
	// Each layer (persona, memory, time, etc.) lives in its own file under
	// layers/ and auto-registers via init(). StreamChat selects only the
	// layers relevant to the conversational model (not the agent planner).
	chatLayerCtx := &layers.LayerContext{
		Store:               ctx.Store,
		Cfg:                 ctx.Cfg,
		EmbedClient:         ctx.EmbedClient,
		RelevantMemories:    ctx.RelevantMemories,
		AgentPassedMemories: ctx.AgentPassedMemories,
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
		// Pass injected memories observability to the TUI.
		for _, m := range lr.InjectedMemories {
			memArgs := fmt.Sprintf("#%d %s", m.ID, m.Source)
			if m.Distance > 0 {
				memArgs = fmt.Sprintf("#%d %s dist=%.2f", m.ID, m.Source, m.Distance)
			}
			if ctx.Phase != nil {
				ctx.Phase.EmitToolCall("memory→chat", memArgs, truncate(m.Content, 80), false)
			} else if ctx.EventBus != nil {
				ctx.EventBus.Emit(tui.ToolCallEvent{
					Time:     time.Now(),
					TurnID:   ctx.TriggerMsgID,
					ToolName: "memory→chat",
					Args:     memArgs,
					Result:   truncate(m.Content, 80),
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

	// Call the conversational model. If a StreamCallback is set (meaning the
	// bot layer has a Telegram message ready to receive live token edits),
	// use the streaming path so tokens flow to the user as they're generated.
	// Otherwise fall back to the plain blocking call — same as before.
	start := time.Now()
	var resp *llm.ChatResponse
	if ctx.StreamCallback != nil {
		resp, err = ctx.ChatLLM.ChatCompletionStreaming(llmMessages, func(token string) {
			_ = ctx.StreamCallback(token)
		})
	} else {
		resp, err = ctx.ChatLLM.ChatCompletion(llmMessages)
	}
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
	replyEvent := tui.ReplyEvent{
		Time:             time.Now(),
		TurnID:           ctx.TriggerMsgID,
		Text:             truncate(resp.Content, 200),
		PromptTokens:     resp.PromptTokens,
		CompletionTokens: resp.CompletionTokens,
		TotalTokens:      resp.TotalTokens,
		CostUSD:          resp.CostUSD,
		LatencyMs:        latencyMs,
	}
	if ctx.Phase != nil {
		ctx.Phase.Emit(replyEvent)
	} else if ctx.EventBus != nil {
		ctx.EventBus.Emit(replyEvent)
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

	// Style gate — two layers, one retry.
	//
	// Layer 1: deterministic pattern check (hasStyleIssue) — free, fast,
	//   catches mechanical AI tics like "not just X, it's Y" and em dashes.
	// Layer 2: LLM classifier (if configured) — catches nuanced patterns
	//   that string matching can't detect.
	//
	// Only the FIRST layer to flag something triggers a retry. This means
	// at most ONE extra generation per turn. Both layers fail-open — the
	// style gate never blocks a reply from reaching the user.
	//
	// styleGateNote appears in the agent trace (and sim report).
	styleGateNote := ""
	var styleHint string
	var styleSource string

	// Layer 1: deterministic check.
	if issue, hint := hasStyleIssue(resp.Content); issue {
		styleHint = hint
		styleSource = "pattern"
	}

	// Layer 2: classifier — only runs if the deterministic check passed
	// AND a classifier is configured. Skipping it when layer 1 already
	// caught something saves the LLM call.
	if styleHint == "" && ctx.ClassifierLLM != nil {
		styleVerdict := classifier.Check(ctx.ClassifierLLM, "reply", resp.Content, nil)
		if styleVerdict.Allowed {
			log.Info("reply: style gate passed")
			styleGateNote = "[style: PASS]"
		} else {
			styleHint = styleVerdict.Reason
			if styleHint == "" {
				styleHint = "avoid formulaic AI openers or closers"
			}
			styleSource = "classifier"
		}
	}

	// Single retry path — shared by both layers.
	if styleHint != "" {
		log.Info("reply: style issue detected, retrying once",
			"source", styleSource, "hint", styleHint,
			"preview", truncate(resp.Content, 80))

		hintMessages := append(llmMessages, llm.ChatMessage{
			Role:    "system",
			Content: "Style note: " + styleHint + ". Rephrase naturally.",
		})
		retryResp, retryErr := ctx.ChatLLM.ChatCompletion(hintMessages)
		if retryErr == nil && !isDegenerate(retryResp.Content) && len(retryResp.Content) <= maxReplyChars {
			resp = retryResp
			ctx.ReplyCost += retryResp.CostUSD
			log.Info("reply: style retry accepted", "source", styleSource, "hint", styleHint,
				"preview", truncate(resp.Content, 80))
			// Report as clean to the agent — the issue was resolved internally.
			// Exposing retry details causes the agent to self-correct in a loop.
			// Full details are in the log line above for observability.
			styleGateNote = "[style: clean]"
		} else {
			log.Warn("reply: style retry failed or invalid, delivering original",
				"source", styleSource, "hint", styleHint)
			// Same here — the original was delivered, so from the agent's
			// perspective the reply is done. No need to signal a problem.
			styleGateNote = "[style: clean]"
		}
	} else if styleGateNote == "" {
		styleGateNote = "[style: clean]"
	}

	// Safety gate — separate classifier focused on emotional safety.
	// Catches escalation, drastic-decision endorsement, and pure
	// sycophantic validation. Independent from style — runs its own
	// LLM call with its own prompt, knows nothing about style issues.
	// Same retry-with-hint pattern: one chance to rephrase, then deliver.
	//
	// We pass the user's message as a snippet so the classifier can judge
	// whether the bot is mirroring the user's temperature or escalating
	// beyond it. Without this, it has no reference for "what did the user
	// actually say?" and over-flags mirroring as escalation.
	safetyGateNote := ""
	if ctx.ClassifierLLM != nil {
		safetySnippet := []memory.Message{{
			Role:            "user",
			ContentScrubbed: ctx.ScrubbedUserMessage,
		}}
		safetyVerdict := classifier.Check(ctx.ClassifierLLM, "reply_safety", resp.Content, safetySnippet)
		if safetyVerdict.Allowed {
			safetyGateNote = "[safety: SAFE]"
		} else {
			safetyHint := safetyVerdict.Reason
			if safetyHint == "" {
				safetyHint = "balance your support with honest perspective"
			}
			log.Info("reply: safety issue detected, retrying once",
				"verdict", safetyVerdict.Type, "hint", safetyHint,
				"preview", truncate(resp.Content, 80))

			safetyMessages := append(llmMessages, llm.ChatMessage{
				Role:    "system",
				Content: "Safety note: " + safetyHint + ". Rephrase to be supportive but balanced.",
			})
			retryResp, retryErr := ctx.ChatLLM.ChatCompletion(safetyMessages)
			if retryErr == nil && !isDegenerate(retryResp.Content) && len(retryResp.Content) <= maxReplyChars {
				resp = retryResp
				ctx.ReplyCost += retryResp.CostUSD
				log.Info("reply: safety retry accepted",
					"verdict", safetyVerdict.Type, "hint", safetyHint,
					"preview", truncate(resp.Content, 80))
				// Report as safe to the agent — the issue was resolved
				// internally. Exposing the verdict type and hint caused
				// the agent to self-correct by calling reply again with
				// a softer instruction, producing near-duplicate messages.
				safetyGateNote = "[safety: SAFE]"
			} else {
				log.Warn("reply: safety retry failed or invalid, delivering original",
					"verdict", safetyVerdict.Type, "hint", safetyHint)
				safetyGateNote = "[safety: SAFE]"
			}
		}
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

	// Deliver the response to Telegram BEFORE saving to the database.
	// This ordering matters: if we save first and then fail to deliver,
	// the message exists in history but the user never saw it — a phantom
	// that poisons the conversation context. Delivery is the fallible part;
	// the DB save is cheap and reliable by comparison.
	//
	// First reply: edit the placeholder message (statusCallback).
	// Follow-up replies: send as a new message (sendCallback) so both
	// are visible — e.g., "let me look that up" → "here's what I found".
	var sendErr error
	if ctx.ReplyCalled && ctx.SendCallback != nil {
		sendErr = ctx.SendCallback(replyText)
	} else if ctx.StatusCallback != nil {
		sendErr = ctx.StatusCallback(replyText)
	}

	if sendErr != nil {
		// Surface the error to the agent so it can see delivery failed.
		// Do NOT save to DB or fire TTS — the message wasn't delivered.
		log.Error("reply: Telegram send failed", "err", sendErr)
		return fmt.Sprintf("error: send failed: %v", sendErr)
	}

	// Stop the typing indicator immediately now that the reply is visible.
	// Without this, typing lingers until agent.Run returns (which involves
	// cleanup, placeholder deletion, etc.) or even longer if there's a race
	// with the typing refresh goroutine.
	if ctx.StopTypingFn != nil {
		ctx.StopTypingFn()
	}

	// Save to DB only after confirmed delivery.
	respID, err := ctx.Store.SaveMessage("assistant", resp.Content, resp.Content, ctx.ConversationID)
	if err != nil {
		log.Error("reply: saving response", "err", err)
	}
	if respID > 0 {
		ctx.Store.UpdateMessageTokenCount(respID, resp.CompletionTokens)
		ctx.Store.SaveMetric(resp.Model, resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens, resp.CostUSD, latencyMs, respID, resp.UsedFallback)
	}

	// TTS fires only for delivered messages — no point synthesizing audio
	// for a reply the user never received.
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
	suffix := "Your message has been sent. Call done to end your turn unless you have pending work (e.g., a search in progress)."
	if styleGateNote != "" || safetyGateNote != "" {
		gates := strings.TrimSpace(styleGateNote + " " + safetyGateNote)
		suffix = gates + "\n" + suffix
	}
	return fmt.Sprintf("reply delivered to user: %q\n\n%s", preview, suffix)
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

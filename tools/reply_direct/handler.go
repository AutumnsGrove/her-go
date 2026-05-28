// Package reply_direct implements the reply_direct tool — delivers text
// written directly by the driver agent, bypassing the chat model entirely.
//
// This is an experimental alternative to the standard reply tool. The
// driver agent writes the actual words the user sees, instead of writing
// instructions for a separate chat model to interpret. Style and safety
// gates still run; PII deanonymization still happens.
//
// Gated by config: driver.direct_reply must be true AND (direct_reply_sim_only
// must be false OR we must be in a sim run).
package reply_direct

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"her/classifier"
	"her/logger"
	"her/memory"
	"her/retry"
	"her/scrub"
	"her/tools"
	"her/tui"
)

var log = logger.WithPrefix("tools/reply_direct")

func init() {
	tools.Register("reply_direct", Handle)
}

// Handle delivers driver-authored text directly to the user. Same delivery
// pipeline as the standard reply (Telegram send, pagination, TTS, DB save)
// but skips the chat LLM call and layer assembly entirely.
func Handle(argsJSON string, ctx *tools.Context) string {
	// Gate check: direct reply must be enabled.
	if ctx.Cfg != nil && !ctx.Cfg.Driver.DirectReply {
		return "error: reply_direct is not enabled. Use the standard reply tool instead."
	}

	// Sim-only guard: if direct_reply_sim_only is true (default), only
	// allow in sim runs.
	if ctx.Cfg != nil {
		simOnly := ctx.Cfg.Driver.DirectReplySimOnly
		if simOnly == nil || *simOnly {
			if ctx.AgentName != "sim" && !ctx.IsSimRun {
				return "error: reply_direct is restricted to sim runs. Set direct_reply_sim_only: false to use in production."
			}
		}
	}

	// Reply count guard — same cap as the standard reply tool.
	maxReplies := 2
	if ctx.Cfg != nil && ctx.Cfg.Driver.MaxRepliesPerTurn > 0 {
		maxReplies = ctx.Cfg.Driver.MaxRepliesPerTurn
	}
	if ctx.ReplyCount >= maxReplies {
		return fmt.Sprintf(
			"rejected: you already sent %d replies this turn (max %d). Call done now.",
			ctx.ReplyCount, maxReplies)
	}

	var args struct {
		Text      string  `json:"text"`
		MemoryIDs []int64 `json:"memory_ids"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	if strings.TrimSpace(args.Text) == "" {
		return "error: text cannot be empty"
	}

	// Track memory usage for blended retrieval scoring.
	if len(args.MemoryIDs) > 0 && ctx.Store != nil {
		if err := ctx.Store.MarkMemoriesRecalled(args.MemoryIDs); err != nil {
			log.Warn("reply_direct: failed to mark memories recalled", "err", err)
		}
	}

	replyText := args.Text

	// Style gate — same deterministic check as the standard reply.
	// We don't have a chat model to retry with, so if the gate fires
	// we return an error asking the driver to rephrase.
	if ctx.ClassifierLLM != nil {
		styleVerdict := classifier.Check(ctx.ClassifierLLM, "reply", replyText, nil)
		if styleVerdict.Model != "" && ctx.Store != nil {
			ctx.Store.SaveMetric(styleVerdict.Model, styleVerdict.PromptTokens, styleVerdict.CompletionTokens, styleVerdict.TotalTokens, styleVerdict.CostUSD, 0, ctx.TriggerMsgID, false, memory.RoleClassifier)
		}
		if !styleVerdict.Allowed {
			return fmt.Sprintf("rejected (style): %s. Rephrase your text and try again.", styleVerdict.Reason)
		}
	}

	// Safety gate.
	if ctx.ClassifierLLM != nil {
		safetySnippet := []memory.Message{{
			Role:            "user",
			ContentScrubbed: ctx.ScrubbedUserMessage,
		}}
		safetyVerdict := classifier.Check(ctx.ClassifierLLM, "reply_safety", replyText, safetySnippet)
		if safetyVerdict.Model != "" && ctx.Store != nil {
			ctx.Store.SaveMetric(safetyVerdict.Model, safetyVerdict.PromptTokens, safetyVerdict.CompletionTokens, safetyVerdict.TotalTokens, safetyVerdict.CostUSD, 0, ctx.TriggerMsgID, false, memory.RoleClassifier)
		}
		if !safetyVerdict.Allowed {
			return fmt.Sprintf("rejected (safety): %s. Rephrase to be supportive but balanced.", safetyVerdict.Reason)
		}
	}

	// PII deanonymization.
	replyText = scrub.Deanonymize(replyText, ctx.ScrubVault)

	// Duplicate guard.
	if ctx.ReplyCalled && replyText == ctx.ReplyText {
		return "reply skipped (duplicate of previous reply)"
	}

	// Deliver.
	var sendErr error
	sendRetry := retry.Config{
		MaxAttempts: 3,
		Backoff:     retry.Exponential,
		InitialWait: 500 * time.Millisecond,
	}
	sendCtx := ctx.Ctx
	if sendCtx == nil {
		sendCtx = context.Background()
	}

	if len(replyText) > tools.TelegramMaxMessageLen && ctx.SendPaginatedCallback != nil {
		if !ctx.ReplyCalled && ctx.DeletePlaceholderCallback != nil {
			_ = ctx.DeletePlaceholderCallback()
		}
		sendErr = retry.Do(sendCtx, sendRetry, func() error {
			return ctx.SendPaginatedCallback(replyText)
		})
	} else {
		if ctx.ReplyCalled && ctx.SendCallback != nil {
			sendErr = retry.Do(sendCtx, sendRetry, func() error {
				return ctx.SendCallback(replyText)
			})
		} else if ctx.StatusCallback != nil {
			sendErr = retry.Do(sendCtx, sendRetry, func() error {
				return ctx.StatusCallback(replyText)
			})
		}
	}

	if sendErr != nil {
		log.Error("reply_direct: send failed", "err", sendErr)
		return fmt.Sprintf("error: send failed: %v", sendErr)
	}

	if ctx.StopTypingFn != nil {
		ctx.StopTypingFn()
	}

	// Save to DB.
	if ctx.Store != nil {
		respID, err := ctx.Store.SaveMessage("assistant", replyText, replyText, ctx.ConversationID)
		if err != nil {
			log.Error("reply_direct: saving response", "err", err)
		}
		if respID > 0 {
			ctx.Store.SaveMetric("direct", 0, 0, 0, 0, 0, respID, false, memory.RoleChat)
		}
	}

	// TTS.
	if ctx.TTSCallback != nil {
		go ctx.TTSCallback(replyText)
	}

	// Emit reply event.
	replyEvent := tui.ReplyEvent{
		Time:   time.Now(),
		TurnID: ctx.TriggerMsgID,
		Text:   replyText,
	}
	if ctx.Phase != nil {
		ctx.Phase.Emit(replyEvent)
	} else if ctx.EventBus != nil {
		ctx.EventBus.Emit(replyEvent)
	}

	ctx.ReplyCalled = true
	ctx.ReplyCount++
	ctx.ReplyText = replyText

	if ctx.StageResetCallback != nil {
		if err := ctx.StageResetCallback(); err != nil {
			log.Warn("reply_direct: stage reset failed", "err", err)
		} else {
			ctx.ReplyCalled = false
		}
	}

	preview := replyText
	if len(preview) > 300 {
		preview = preview[:300] + "..."
	}
	return fmt.Sprintf("reply delivered to user: %q\n\nCall done to end your turn.", preview)
}

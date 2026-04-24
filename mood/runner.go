package mood

import (
	"context"
	"time"

	"her/memory"
)

// Runner is the bot-side orchestrator that fires RunAgent whenever a
// new conversation turn lands. It bundles the mood deps and config
// once at bot startup so the post-reply goroutine only has to pass
// the conversation id.
//
// Why not just call RunAgent directly from bot? Because building
// Deps requires wiring four LLM clients, a vocab, an embed function,
// and a Propose callback. Doing that at every call site is a lot of
// noise; doing it once at startup keeps the hot path clean.
type Runner struct {
	Deps   Deps
	Config AgentConfig

	// HistoryWindowMultiplier tunes how many raw messages we pull
	// from the store relative to Config.ContextTurns. The store
	// returns user+assistant pairs intermixed; we grab more than the
	// target and let RunAgent trim. Default 2 (covers single-user
	// follow-ups without over-fetching).
	HistoryWindowMultiplier int
}

// RunForConversation pulls the latest turns for a conversation from
// the store, shapes them as mood.Turns, and dispatches to RunAgent.
// Typical caller: a goroutine in the bot's reply handler, fired
// after the user-visible reply has already been sent.
//
// Errors are logged, never returned — the mood agent is best-effort
// and must not block the user-facing reply loop.
func (r *Runner) RunForConversation(ctx context.Context, convID string) Result {
	if convID == "" {
		return Result{Action: ActionDroppedNoSignal, Reason: "empty convID"}
	}
	mult := r.HistoryWindowMultiplier
	if mult <= 0 {
		mult = 2
	}
	ctxTurns := r.Config.ContextTurns
	if ctxTurns <= 0 {
		ctxTurns = 5
	}

	msgs, err := r.Deps.Store.RecentMessages(convID, ctxTurns*mult)
	if err != nil {
		log.Error("mood runner: fetching recent messages", "err", err)
		return Result{Action: ActionErrored, Reason: err.Error()}
	}
	if len(msgs) == 0 {
		return Result{Action: ActionDroppedNoSignal, Reason: "no messages in conversation"}
	}

	// Drop the trailing assistant message if present — that's the
	// current turn's reply, generated moments ago. The mood agent
	// tracks the *user's* mood, so it should see conversation history
	// leading up to (and including) the user's latest message, with
	// prior assistant replies for context. The current reply hasn't
	// been "responded to" yet, so it can't inform user mood.
	if len(msgs) > 0 && msgs[len(msgs)-1].Role == "assistant" {
		msgs = msgs[:len(msgs)-1]
	}

	turns := make([]Turn, 0, len(msgs))
	for _, m := range msgs {
		content := m.ContentScrubbed
		if content == "" {
			// Falls back to raw when scrub didn't run. In that
			// case the agent still sees coherent text; the PII
			// firewall caveat is documented in the plan.
			content = m.ContentRaw
		}
		turns = append(turns, Turn{
			Role:            m.Role,
			ScrubbedContent: content,
			Timestamp:       m.Timestamp,
		})
	}

	// Conversation id is stamped onto whatever entry the agent saves.
	// Keep a shallow copy of Deps so concurrent runs don't clobber.
	deps := r.Deps
	deps.ConversationID = convID
	if deps.Clock == nil {
		deps.Clock = time.Now
	}

	return RunAgent(ctx, deps, r.Config, turns)
}

// RunForConversationWithTimeout is the common production path: fire
// the agent in a bounded context so a slow LLM can't keep a bot
// goroutine alive forever. Matches the memory agent's timeout
// convention.
func (r *Runner) RunForConversationWithTimeout(parent context.Context, convID string, timeout time.Duration) Result {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	return r.RunForConversation(ctx, convID)
}

// RunForConversationWithTrace is like the timeout variant but also
// swaps in a per-turn trace callback. Used by the bot when
// cfg.Driver.Trace is on so each turn's mood agent lights up a named
// slot on the turn's TraceBoard.
func (r *Runner) RunForConversationWithTrace(parent context.Context, convID string, timeout time.Duration, trace func(string) error) Result {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	// Shallow-copy so the callback swap can't race with another
	// concurrent RunForConversation using the same Runner.
	r2 := *r
	r2.Deps.Trace = trace
	return r2.RunForConversation(ctx, convID)
}

// Assert that Runner's dep list is the minimal surface a bot has to
// wire up — any new required field should show up here so we can
// delete it from the list if we later remove it.
var _ = memory.MoodKindMomentary // anchor import for future typed APIs

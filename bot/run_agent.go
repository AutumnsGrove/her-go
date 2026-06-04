// run_agent.go — Shared agent pipeline for all message handlers.
//
// Before this file existed, every handler (text, photo, voice, location)
// copied ~80 lines of identical boilerplate: typing indicator, placeholder
// message, callback wiring, agent.Run, error handling, TUI events. This
// file extracts that into two reusable pieces:
//
//   - runAgent: the full interactive pipeline (typing, placeholder, traces,
//     all callbacks, TUI events). Used by user-facing message handlers.
//   - baseRunParams: pre-fills the 15+ constant fields from Bot. Used by
//     both runAgent and the simpler event/callback handlers that don't need
//     the full UI machinery.
package bot

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"her/agent"
	"her/classifier"
	"her/memory"
	"her/scrub"
	"her/tools"
	"her/turn"
)

// AgentInput holds the variable parts of an interactive agent run.
// Everything constant (LLM clients, store, config, etc.) comes from the Bot
// struct automatically. Think of this as the "what's different about THIS
// particular message" — the rest is infrastructure.
//
// In Python terms, this is like a dataclass for keyword arguments:
//
//	@dataclass
//	class AgentInput:
//	    user_message: str
//	    conversation_id: str
//	    ...
type AgentInput struct {
	// UserMessage is the original user text (or synthetic prompt for
	// locations/events). Used in logs and TUI events.
	UserMessage string

	// ScrubbedText is the PII-scrubbed version sent to the agent.
	// If empty, UserMessage is used as-is (no scrubbing needed).
	ScrubbedText string

	// ScrubVault holds PII tokens for deanonymization in replies.
	// Nil means an empty vault is created automatically.
	ScrubVault *scrub.Vault

	ConversationID string
	TriggerMsgID   int64

	// Media fields — only populated by the photo handler.
	ImageBase64 string
	ImageMIME   string
	OCRText     string

	// PlaceholderText overrides the default 💭 placeholder message.
	// The voice handler uses this to show the transcript while thinking.
	PlaceholderText string

	// PlaceholderHTML enables HTML parse mode on the placeholder and
	// subsequent status edits. Needed when PlaceholderText contains
	// HTML formatting (e.g., the voice handler's <i>transcript</i>).
	PlaceholderHTML bool

	// ForceTTS enables TTS regardless of the configured reply mode.
	// The voice handler sets this so voice memos always get voice replies,
	// even when reply_mode isn't "voice".
	ForceTTS bool
}

// runAgent runs the full interactive agent pipeline: typing indicator,
// placeholder message, trace callback, all Telegram callbacks, TUI events,
// and error handling. This is what every user-facing message handler calls
// after doing its input-specific prep work (downloading photos, transcribing
// voice, scrubbing PII, etc.).
//
// The flow:
//  1. Start typing indicator (refreshed every 4s)
//  2. Send trace placeholder (if enabled) — appears above the reply
//  3. Send reply placeholder (💭)
//  4. Build all callbacks (status, send, stage reset, confirm, TTS)
//  5. Emit TurnStartEvent for the TUI
//  6. Run agent.Run synchronously
//  7. Clean up (stop typing, emit TurnEndEvent)
//  8. Handle errors (edit placeholder with error message)
func (b *Bot) runAgent(fe Frontend, input AgentInput) error {
	// Prevent concurrent agent runs. Two simultaneous runs would race
	// for the same conversation state.
	if b.agentBusy.Load() {
		log.Info("agent busy, ignoring concurrent message", "msg", truncate(input.UserMessage, 60))
		return fe.SendBusy()
	}

	// Apply defaults for optional fields.
	scrubbedText := input.ScrubbedText
	if scrubbedText == "" {
		scrubbedText = input.UserMessage
	}
	vault := input.ScrubVault
	if vault == nil {
		vault = scrub.NewVault()
	}

	// --- Typing indicator ---
	stopTypingFn := fe.StartTyping()

	// --- Turn tracker ---
	tracker := turn.NewTracker(
		input.TriggerMsgID,
		b.eventBus,
		stopTypingFn,
		truncate(input.UserMessage, 100),
		input.ConversationID,
	)
	defer tracker.StopTyping()

	// --- Trace callbacks ---
	var traceCallback tools.TraceCallback
	var memoryTraceCallback tools.TraceCallback
	var moodTraceCallback tools.TraceCallback
	var personaTraceCallback tools.TraceCallback
	var introspectionTraceCallback tools.TraceCallback
	var traceFinalize func()
	var liteToolHook func(toolName string)
	var substanceTrace tools.TraceCallback

	// Traces flow through the TraceProvider optional interface.
	// Both TelegramFrontend and gatewayFrontend implement it —
	// Telegram renders into an editable message, gateway routes
	// to SSE streams / adapter panels.
	//
	// Two modes:
	//   Full (/traces ON):  detailed per-tool-call traces passed into agents
	//   Lite (/traces OFF): compact progress summaries written from here
	//
	// The trace board + message are ALWAYS created — lite mode gives
	// visibility into the pipeline without the verbose tool-by-tool output.

	// liteTrace writes to a slot on the trace board. Used in lite mode
	// to show compact progress lines without passing callbacks into agents.
	var liteTrace func(slot, text string)

	if tp, ok := fe.(TraceProvider); ok {
		traceFinalize = tp.TraceFinalize

		substanceTrace = tp.TraceCallback("substance")

		if b.cfg.Driver.Trace {
			// Full mode — wire verbose callbacks into agents.
			traceCallback = tp.TraceCallback("main")
			memoryTraceCallback = tp.TraceCallback("memory")
			moodTraceCallback = tp.TraceCallback("mood")
			personaTraceCallback = tp.TraceCallback("persona")
			introspectionTraceCallback = tp.TraceCallback("introspection")
		} else {
			// Lite mode — agents get no verbose callbacks, but we wire
			// a lightweight main callback that progressively renders the
			// tool sequence as an arrow chain ("think → recall → reply → done").
			// The agent loop calls TraceCallback after each tool call with
			// the full trace text — we ignore that and render toolSeq instead.
			memoryTraceCallback = tp.TraceCallback("memory")
			mainSlot := tp.TraceCallback("main")

			var liteSeq []string
			var liteMu sync.Mutex
			traceCallback = func(text string) error {
				// Not used for content — we render from liteSeq instead.
				return nil
			}
			liteTrace = func(slot, text string) {
				tp.TraceCallback(slot)(text)
			}

			// LiteToolHook is called from agent.Run after each tool execution.
			// It appends the tool name and re-renders the main slot.
			liteToolHook = func(toolName string) {
				liteMu.Lock()
				liteSeq = append(liteSeq, toolName)
				line := "🛠️ " + strings.Join(liteSeq, " → ")
				liteMu.Unlock()
				mainSlot(line)
			}
		}
	}
	_ = personaTraceCallback

	// --- Reply placeholder ---
	placeholderText := "\U0001F4AD"
	if input.PlaceholderText != "" {
		placeholderText = input.PlaceholderText
	}

	if err := fe.SendPlaceholder(placeholderText, input.PlaceholderHTML); err != nil {
		tracker.StopTyping()
		log.Error("sending placeholder", "err", err)
		return fe.SendError("Sorry, I'm having trouble right now. Try again in a moment?")
	}

	// --- Build callbacks from the Frontend interface ---
	statusCallback := func(status string) error { return fe.EditStatus(status) }
	sendCallback := func(text string) error { return fe.SendReply(text) }
	sendConfirmCallback := func(text string) (int64, error) { return fe.SendConfirm(text) }
	stageResetCallback := func() error { return fe.StageReset() }
	deletePlaceholderCallback := func() error { return fe.DeletePlaceholder() }
	sendPaginatedCallback := func(text string) error { return fe.SendPaginated(text) }

	// TTS callback — any frontend implementing TTSProvider gets voice replies.
	var ttsCallback tools.TTSCallback
	if tp, ok := fe.(TTSProvider); ok {
		if b.ttsClient != nil && (input.ForceTTS || b.ttsClient.ReplyMode() == "voice") {
			ttsCallback = func(text string) {
				tp.SendVoice(text)
			}
		}
	}

	// Stream callback — any frontend implementing StreamProvider gets
	// token-level streaming with its own buffering strategy.
	var streamCallback tools.StreamCallback
	var stopStream func()
	if fe.SupportsStreaming() {
		if sp, ok := fe.(StreamProvider); ok {
			streamCallback, stopStream = sp.MakeStreamCallback()
		}
	}

	// --- Run the agent ---
	params := b.baseRunParams()
	params.Tracker = tracker
	params.ScrubbedUserMessage = scrubbedText
	params.ScrubVault = vault
	params.ConversationID = input.ConversationID
	params.TriggerMsgID = input.TriggerMsgID
	params.StatusCallback = statusCallback
	params.SendCallback = sendCallback
	params.StageResetCallback = stageResetCallback
	params.DeletePlaceholderCallback = deletePlaceholderCallback
	params.SendConfirmCallback = sendConfirmCallback
	params.TTSCallback = ttsCallback
	params.StreamCallback = streamCallback
	params.SendPaginatedCallback = sendPaginatedCallback
	params.TraceCallback = traceCallback
	params.MemoryTraceCallback = memoryTraceCallback
	params.ImageBase64 = input.ImageBase64
	params.ImageMIME = input.ImageMIME
	params.OCRText = input.OCRText
	params.LiteToolHook = liteToolHook
	params.IsSimRun = b.isSimRun

	b.agentBusy.Store(true)
	result, err := agent.Run(params)
	b.agentBusy.Store(false)

	if stopStream != nil {
		stopStream()
	}

	if err != nil {
		tracker.StopTyping()
		log.Error("agent error", "err", err)
		_ = fe.SendError("Sorry, I'm having trouble thinking right now. Try again in a moment?")
		return nil
	}

	log.Infof("  %s: %s", strings.ToLower(b.cfg.Identity.Her), truncate(result.ReplyText, 100))
	log.Info("─── reply sent ───")

	// In lite mode, the driver tool sequence was already rendered
	// progressively by liteToolHook during agent.Run().

	// --- Substance gate + batching ---
	// Instead of firing all three background agents on every turn, we
	// check whether this exchange has enough substance to warrant analysis.
	// Casual turns ("lol", "ok", "thanks") get buffered; the batch fires
	// when either a substantive turn arrives or the counter hits threshold.
	//
	// This is like a debounce — low-value turns are cheap to skip, but
	// we guarantee nothing is lost by forcing a batch periodically.
	threshold := b.cfg.BackgroundAgents.BatchThreshold
	if threshold <= 0 {
		threshold = 3 // default: batch every 3 skipped turns
	}

	shouldAnalyze := true
	if b.cfg.BackgroundAgents.SubstanceGate && b.classifierLLM != nil {
		shouldAnalyze = classifier.CheckSubstance(b.classifierLLM, input.UserMessage, result.ReplyText)
		if shouldAnalyze {
			log.Info("substance gate: ANALYZE")
		} else {
			log.Info("substance gate: SKIP")
		}
	}

	// Build the current turn's pending data.
	currentTurn := PendingTurn{
		UserMessage:    scrubbedText,
		ReplyText:      result.ReplyText,
		ThinkTraces:    result.ThinkTraces,
		TriggerMsgID:   input.TriggerMsgID,
		ConversationID: input.ConversationID,
	}

	// Append to buffer and check whether to fire.
	b.pendingMu.Lock()
	b.pendingTurns = append(b.pendingTurns, currentTurn)
	count := int(b.turnCounter.Add(1))
	b.pendingMu.Unlock()

	if shouldAnalyze || count >= threshold {
		// Drain the buffer — run background agents on the accumulated batch.
		b.pendingMu.Lock()
		turns := b.pendingTurns
		b.pendingTurns = nil
		b.turnCounter.Store(0)
		b.pendingMu.Unlock()

		// Emit substance decision to its own trace slot.
		if substanceTrace != nil {
			if shouldAnalyze {
				substanceTrace(fmt.Sprintf("⚡ substance: ANALYZE — processing %d turn(s)", len(turns)))
			} else {
				substanceTrace(fmt.Sprintf("⚡ substance: threshold hit (%d/%d) — processing batch", count, threshold))
			}
		}

		b.launchBackgroundAgents(turns, result, params, tracker,
			memoryTraceCallback, moodTraceCallback, introspectionTraceCallback, liteTrace)
	} else {
		if substanceTrace != nil {
			substanceTrace(fmt.Sprintf("⚡ substance: SKIP — deferred (%d/%d)", count, threshold))
		}
		log.Info("turn batched — background agents deferred",
			"pending", count, "threshold", threshold)
	}

	// Emit total cost after all phases complete, then finalize the trace.
	// Both full and lite modes get this — it's the last line on the board.
	if traceFinalize != nil || liteTrace != nil {
		go func() {
			tracker.Wait()
			m := tracker.Metrics()
			costLine := fmt.Sprintf("💰 $%.4f · %s", m.TotalCost, tracker.Elapsed().Round(time.Millisecond))
			if tp, ok := fe.(TraceProvider); ok {
				tp.TraceCallback("cost")(costLine)
			}
			if traceFinalize != nil {
				traceFinalize()
			}
		}()
	}

	return nil
}

// launchBackgroundAgents fires memory, mood, and introspection agents
// for a batch of accumulated turns. The memory agent runs once on the
// latest turn's context (it reads recent messages from the DB, so all
// accumulated turns are visible in its conversation window). Mood and
// introspection also run once on the latest turn.
//
// The sync.WaitGroup coordinates ordering: memory + mood finish first,
// then introspection runs (it needs to see any self-memories the memory
// agent just wrote). This is the same coordination that was previously
// split between agent.Run() and runAgent — now it lives in one place.
func (b *Bot) launchBackgroundAgents(
	turns []PendingTurn,
	latestResult *agent.RunResult,
	latestParams agent.RunParams,
	tracker *turn.Tracker,
	memoryTrace tools.TraceCallback,
	moodTrace tools.TraceCallback,
	introspectionTrace tools.TraceCallback,
	liteTrace func(slot, text string),
) {
	if len(turns) == 0 {
		return
	}
	latest := turns[len(turns)-1]
	var introWG sync.WaitGroup

	// --- Memory agent ---
	if b.memoryAgentLLM != nil {
		var memPhase *turn.PhaseHandle
		if tracker != nil {
			memPhase = tracker.Begin("memory")
		}
		introWG.Add(1)

		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Error("memory agent panic (recovered)", "panic", r)
				}
			}()
			defer introWG.Done()
			if memPhase != nil {
				defer memPhase.Done(turn.PhaseMetrics{})
			}
			result := agent.RunMemoryAgent(
				agent.MemoryAgentInput{
					UserMessage:    latest.UserMessage,
					ThinkTraces:    latest.ThinkTraces,
					ReplyText:      latest.ReplyText,
					TriggerMsgID:   latest.TriggerMsgID,
					ConversationID: latest.ConversationID,
				},
				agent.MemoryAgentParams{
					LLM:           b.memoryAgentLLM,
					ClassifierLLM: b.classifierLLM,
					Store:         b.store,
					EmbedClient:   b.embedClient,
					Cfg:           b.cfg,
					TraceCallback: memoryTrace,
					EventBus:      b.eventBus,
					AgentEventCB:  latestParams.AgentEventCB,
					Phase:         memPhase,
				},
			)
			if liteTrace != nil {
				liteTrace("memory", fmt.Sprintf("🧩 memory ✓ %d saved", result.MemoriesSaved))
			}
		}()
	}

	// --- Mood + introspection ---
	b.launchMoodAgent(latest.ConversationID, moodTrace, tracker, &introWG)
	b.launchIntrospectionAgent(latestResult, latestParams, &introWG, introspectionTrace, tracker)

	// In lite mode, emit a mood summary once everything finishes.
	if liteTrace != nil {
		go func() {
			tracker.Wait()
			if entry, err := b.store.LatestMoodEntry(memory.MoodKindMomentary); err == nil && entry != nil {
				liteTrace("mood", fmt.Sprintf("🎭 mood: %s", strings.Join(entry.Labels, ", ")))
			}
		}()
	}
}

// baseRunParams returns a RunParams pre-filled with all the constant fields
// from the Bot struct. The 15+ fields that are identical across every agent
// run (LLM clients, store, config, thresholds, etc.) are set once here.
//
// Callers set the variable fields (message, vault, conversation ID, callbacks)
// before passing to agent.Run. Used by both runAgent (for the full interactive
// pipeline) and simpler handlers like handleAgentEvent that don't need the
// full UI machinery.
func (b *Bot) baseRunParams() agent.RunParams {
	return agent.RunParams{
		DriverLLM:           b.driverLLM,
		MemoryAgentLLM:      b.memoryAgentLLM,
		ChatLLM:             b.llm,
		VisionLLM:           b.visionLLM,
		ClassifierLLM:       b.classifierLLM,
		Store:               b.store,
		EmbedClient:         b.embedClient,
		SimilarityThreshold: b.cfg.Embed.SimilarityThreshold,
		TavilyClient:        b.tavilyClient,
		CalendarBridge:      b.calendarBridge,
		Cfg:                 b.cfg,
		EventBus:            b.eventBus,
		ConfigPath:          b.configPath,
		// Wire the agent event callback so the memory agent's notify_agent
		// tool can wake up the driver agent for a follow-up message.
		// Non-blocking send: if the channel is full (16 buffered events),
		// drop the event rather than deadlocking the memory agent goroutine.
		AgentEventCB: func(summary, directMessage string) {
			if b.agentEventsStopped.Load() {
				return
			}
			evt := agent.AgentEvent{
				Type:          agent.EventInboxReady,
				Summary:       summary,
				DirectMessage: directMessage,
				Timestamp:     time.Now(),
			}
			select {
			case b.agentEvents <- evt:
			default:
				log.Warn("agent event channel full, dropping inbox-ready event",
					"summary", summary)
			}
		},
	}
}

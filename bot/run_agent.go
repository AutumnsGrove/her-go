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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"her/agent"
	"her/classifier"
	"her/mood"
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

	// liteState accumulates compact progress data. A single "lite" slot
	// on the trace board gets re-rendered as each piece arrives, producing
	// a tight 2-4 line block with no separators or bold headers.
	var lite *liteTraceState

	if tp, ok := fe.(TraceProvider); ok {
		traceFinalize = tp.TraceFinalize

		if b.cfg.Driver.Trace {
			// Full mode — wire verbose callbacks into agents.
			traceCallback = tp.TraceCallback("main")
			substanceTrace = tp.TraceCallback("substance")
			memoryTraceCallback = tp.TraceCallback("memory")
			moodTraceCallback = tp.TraceCallback("mood")
			personaTraceCallback = tp.TraceCallback("persona")
			introspectionTraceCallback = tp.TraceCallback("introspection")
		} else {
			// Lite mode — all content goes into one "lite" slot on the board.
			// No bold headers, no separators. Just compact progress lines.
			liteSlot := tp.TraceCallback("lite")
			lite = &liteTraceState{render: func(text string) { liteSlot(text) }}

			// No-op traceCallback so agent.Run sees a non-nil callback
			// (needed for the tracing flag) but doesn't render verbose output.
			traceCallback = func(text string) error { return nil }

			liteToolHook = func(toolName string) {
				lite.addTool(toolName)
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

	// --- Fast-path check ---
	// Before spinning up the full driver agent, ask a cheap classifier
	// whether this message can be handled directly by the chat model.
	// Simple conversational turns (greetings, banter, reactions) skip the
	// driver entirely — saves 3-5 LLM calls per turn.
	if b.shouldFastPath(input) {
		if verdict := b.classifyRoute(scrubbedText, input.ConversationID); verdict == "SKIP" {
			b.agentBusy.Store(true)
			fpResult, fpErr := b.runFastPath(fe, input, scrubbedText, vault, tracker,
				statusCallback, streamCallback, ttsCallback)
			b.agentBusy.Store(false)

			if stopStream != nil {
				stopStream()
			}
			tracker.StopTyping()

			if fpErr != nil {
				log.Error("fast-path error", "err", fpErr)
				_ = fe.SendError("Sorry, I'm having trouble right now. Try again in a moment?")
				return nil
			}

			log.Info("─── fast-path reply sent ───")

			// Feed into the substance gate + batching system so background
			// agents (memory, mood, introspection) still process this turn.
			currentTurn := PendingTurn{
				UserMessage:    scrubbedText,
				ReplyText:      fpResult.replyText,
				TriggerMsgID:   input.TriggerMsgID,
				ConversationID: input.ConversationID,
			}

			b.pendingMu.Lock()
			b.pendingTurns = append(b.pendingTurns, currentTurn)
			count := int(b.turnCounter.Add(1))
			b.pendingMu.Unlock()

			// Fast-path turns always count toward the batch threshold.
			// The substance gate is skipped here — we already know this
			// is a low-substance turn (that's why it was SKIP'd).
			threshold := b.cfg.BackgroundAgents.BatchThreshold
			if threshold <= 0 {
				threshold = 3
			}

			if count >= threshold {
				b.pendingMu.Lock()
				turns := b.pendingTurns
				b.pendingTurns = nil
				b.turnCounter.Store(0)
				b.pendingMu.Unlock()

				if substanceTrace != nil {
					substanceTrace(fmt.Sprintf("⚡ fast-path batch: threshold hit (%d/%d) — processing %d turn(s)", count, threshold, len(turns)))
				}
				if lite != nil {
					lite.setSubstance(fmt.Sprintf("⚡ threshold (%d/%d) — batch", count, threshold))
				}

				b.launchBackgroundAgents(turns, nil, b.baseRunParams(), tracker,
					memoryTraceCallback, moodTraceCallback, introspectionTraceCallback, lite)
			} else {
				if substanceTrace != nil {
					substanceTrace(fmt.Sprintf("⚡ fast-path: deferred (%d/%d)", count, threshold))
				}
				if lite != nil {
					lite.setSubstance(fmt.Sprintf("⚡ SKIP (%d/%d)", count, threshold))
				}
				log.Info("fast-path turn batched — background agents deferred",
					"pending", count, "threshold", threshold)
			}

			// Finalize traces (cost + timing).
			if traceFinalize != nil || lite != nil {
				go func() {
					tracker.Wait()
					m := tracker.Metrics()
					elapsed := tracker.Elapsed().Round(time.Millisecond)
					costLine := fmt.Sprintf("💰 $%.4f · %s", m.TotalCost, elapsed)
					if lite != nil {
						lite.setCost(costLine)
					}
					if tp, ok := fe.(TraceProvider); ok && b.cfg.Driver.Trace {
						tp.TraceCallback("cost")(costLine)
					}
					time.Sleep(500 * time.Millisecond)
					if traceFinalize != nil {
						traceFinalize()
					}
				}()
			}

			return nil
		}
	}

	// --- Run the agent (full pipeline) ---
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

	// If narrate_report queued a report for voice narration, synthesize
	// and deliver it. Two paths:
	//   - Telegram: send as a voice memo via SendVoice
	//   - Sim/dev: save as an OGG file next to the reports
	// The small delay lets the reply TTS (which runs in a goroutine
	// from the reply tool) get a head start so messages arrive in order.
	if result.PendingNarration != "" && b.ttsClient != nil {
		go func() {
			time.Sleep(2 * time.Second)
			log.Info("narrating report", "chars", len(result.PendingNarration))

			if tp, ok := fe.(interface{ SendVoice(string) }); ok {
				// Telegram path — send as voice memo.
				tp.SendVoice(result.PendingNarration)
			} else {
				// Sim/dev path — synthesize and save to file.
				oggBytes, err := b.ttsClient.Synthesize(result.PendingNarration)
				if err != nil {
					log.Error("narration synthesis failed", "err", err)
					return
				}
				narrationPath := filepath.Join(b.reportsDir(), "narration.ogg")
				if err := os.WriteFile(narrationPath, oggBytes, 0644); err != nil {
					log.Error("saving narration file", "err", err)
					return
				}
				log.Info("narration saved", "path", narrationPath, "bytes", len(oggBytes))
			}
		}()
	}

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

		// Substance + background agents.
		if substanceTrace != nil {
			if shouldAnalyze {
				substanceTrace(fmt.Sprintf("⚡ substance: ANALYZE — processing %d turn(s)", len(turns)))
			} else {
				substanceTrace(fmt.Sprintf("⚡ substance: threshold hit (%d/%d) — processing batch", count, threshold))
			}
		}
		if lite != nil {
			if shouldAnalyze {
				lite.setSubstance(fmt.Sprintf("⚡ ANALYZE — %d turn(s)", len(turns)))
			} else {
				lite.setSubstance(fmt.Sprintf("⚡ threshold (%d/%d) — batch", count, threshold))
			}
		}

		b.launchBackgroundAgents(turns, result, params, tracker,
			memoryTraceCallback, moodTraceCallback, introspectionTraceCallback, lite)
	} else {
		if substanceTrace != nil {
			substanceTrace(fmt.Sprintf("⚡ substance: SKIP — deferred (%d/%d)", count, threshold))
		}
		if lite != nil {
			lite.setSubstance(fmt.Sprintf("⚡ SKIP (%d/%d)", count, threshold))
		}
		log.Info("turn batched — background agents deferred",
			"pending", count, "threshold", threshold)
	}

	// Emit total cost after all phases complete, then finalize the trace.
	// The small sleep before finalize ensures async board edits from
	// setCost have time to propagate to Telegram.
	if traceFinalize != nil || lite != nil {
		go func() {
			tracker.Wait()
			m := tracker.Metrics()
			elapsed := tracker.Elapsed().Round(time.Millisecond)
			costLine := fmt.Sprintf("💰 $%.4f · %s", m.TotalCost, elapsed)
			log.Info("turn complete", "cost", costLine)
			if lite != nil {
				lite.setCost(costLine)
			}
			if tp, ok := fe.(TraceProvider); ok && b.cfg.Driver.Trace {
				tp.TraceCallback("cost")(costLine)
			}
			// Let async board edits settle before finalizing.
			time.Sleep(500 * time.Millisecond)
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
	lite *liteTraceState,
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
			if lite != nil {
				lite.setMemory(fmt.Sprintf("🧩 %d saved", result.MemoriesSaved))
			}
		}()
	}

	// --- Mood + introspection ---
	// Pass a callback so the mood agent's actual result drives the lite
	// trace — not a stale DB read that shows yesterday's mood.
	var liteMoodFn func(mood.Result)
	if lite != nil {
		liteMoodFn = func(res mood.Result) {
			if res.Inference != nil && len(res.Inference.Labels) > 0 {
				lite.setMood(fmt.Sprintf("🎭 %s (v%d)", strings.Join(res.Inference.Labels, ", "), res.Inference.Valence))
			} else {
				lite.setMood("🎭 no mood")
			}
		}
	}
	b.launchMoodAgent(latest.ConversationID, moodTrace, tracker, &introWG, liteMoodFn)
	b.launchIntrospectionAgent(latestResult, latestParams, &introWG, introspectionTrace, tracker, lite)
}

// liteTraceState accumulates compact progress data for lite trace mode.
// All fields are set by different goroutines; the mutex protects them.
// Each setter calls flush() which re-renders the single "lite" slot.
type liteTraceState struct {
	mu            sync.Mutex
	tools         []string
	substance     string
	memResult     string
	introspection string
	mood          string
	cost          string
	render        func(string) // writes to the "lite" board slot
}

func (s *liteTraceState) flush() {
	var lines []string
	if len(s.tools) > 0 {
		lines = append(lines, "🛠️ "+strings.Join(s.tools, " → "))
	}
	if s.substance != "" {
		lines = append(lines, s.substance)
	}
	var agents []string
	if s.memResult != "" {
		agents = append(agents, s.memResult)
	}
	if s.introspection != "" {
		agents = append(agents, s.introspection)
	}
	if s.mood != "" {
		agents = append(agents, s.mood)
	}
	if len(agents) > 0 {
		lines = append(lines, strings.Join(agents, " · "))
	}
	if s.cost != "" {
		lines = append(lines, s.cost)
	}
	s.render(strings.Join(lines, "\n"))
}

func (s *liteTraceState) addTool(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools = append(s.tools, name)
	s.flush()
}

func (s *liteTraceState) setSubstance(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.substance = text
	s.flush()
}

func (s *liteTraceState) setMemory(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.memResult = text
	s.flush()
}

func (s *liteTraceState) setIntrospection(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.introspection = text
	s.flush()
}

func (s *liteTraceState) setMood(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mood = text
	s.flush()
}

func (s *liteTraceState) setCost(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cost = text
	s.flush()
	s.logSummary()
}

// logSummary writes the final lite trace state to the server log so
// mood/memory/introspection outcomes are visible in journalctl.
func (s *liteTraceState) logSummary() {
	var parts []string
	if s.substance != "" {
		parts = append(parts, s.substance)
	}
	if s.memResult != "" {
		parts = append(parts, s.memResult)
	}
	if s.mood != "" {
		parts = append(parts, s.mood)
	}
	if s.introspection != "" {
		parts = append(parts, s.introspection)
	}
	if len(parts) > 0 {
		log.Info("lite trace summary", "detail", strings.Join(parts, " · "))
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
		ReportsDir:          b.reportsDir(),
		WorkerCallback:      b.workerCallback,
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

// reportsDir returns the absolute path to the reports/ directory,
// resolved relative to the config file's parent directory (project root).
func (b *Bot) reportsDir() string {
	return filepath.Join(filepath.Dir(b.configPath), "reports")
}

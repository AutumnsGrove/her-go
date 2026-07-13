package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	engine "her/agent_engine"
	"her/calendar"
	"her/gmail"
	"her/layers"
	"her/compact"
	"her/config"
	"her/embed"
	"her/llm"
	"her/logger"
	"her/memory"
	"her/scrub"
	"her/search"
	"her/tools"
	"her/trace"
	"her/tui"
	"her/turn"

	// Blank imports trigger init() registration for each tool's handler.
	// Same pattern as database drivers: import _ "github.com/lib/pq"
	// Each import causes the package's init() to run, which calls
	// tools.Register("name", Handle) to add the handler to the registry.
	_ "her/tools/calendar_create"
	_ "her/tools/calendar_delete"
	_ "her/tools/calendar_list"
	_ "her/tools/calendar_update"
	_ "her/tools/create_schedule"
	_ "her/tools/delete_schedule"
	_ "her/tools/done"
	_ "her/tools/get_time"
	_ "her/tools/get_weather"
	_ "her/tools/list_calendars"
	_ "her/tools/list_files"
	_ "her/tools/list_schedules"
	_ "her/tools/narrate_report"
	_ "her/tools/nearby_search"
	_ "her/tools/publish_report"
	_ "her/tools/read_file"
	_ "her/tools/recall_memories"
	_ "her/tools/reply"
	_ "her/tools/reply_direct"
	_ "her/tools/search_books"
	_ "her/tools/send_task"
	_ "her/tools/set_location"
	_ "her/tools/shift_hours"
	_ "her/tools/think"
	_ "her/tools/update_persona"
	_ "her/tools/update_schedule"
	_ "her/tools/use_tools"
	_ "her/tools/view_image"
	_ "her/tools/web_read"
	_ "her/tools/web_search"
)

// log is the package-level logger for the agent package.
var log = logger.WithPrefix("agent")

// Register this package's trace streams. Main renders first as the
// headline transcript; memory renders below with a 🧩 label. Each
// stream gets its own emoji so the slots are visually distinct at
// a glance — main is the big tool caller, hence the toolbox.
func init() {
	trace.Register(trace.Stream{Name: "main", Order: 100, Label: "🛠️ <b>main</b>"})
	trace.Register(trace.Stream{Name: "substance", Order: 150, Label: ""})
	trace.Register(trace.Stream{Name: "memory", Order: 200, Label: "🧩 <b>memory</b>"})
	trace.Register(trace.Stream{Name: "lite", Order: 50, Label: ""})
	trace.Register(trace.Stream{Name: "cost", Order: 900, Label: ""})

	// Turn phase registration — same pattern as trace streams.
	// "driver" and "memory" register here because this package owns
	// both the driver agent loop and the memory agent launch.
	turn.Register(turn.Phase{Name: "driver", Order: 100, Emoji: "🛠️", Label: "driver"})
	turn.Register(turn.Phase{Name: "memory", Order: 200, Emoji: "🧩", Label: "memory"})
}

// Callback types and toolContext have been moved to the tools package
// (tools/context.go). The agent imports them as tools.Context,
// tools.StatusCallback, etc. See tools/context.go for documentation.

const (
	// defaultAgentPrompt is used as a fallback if main_agent_prompt.md can't be loaded.
	defaultAgentPrompt = `You are {{her}}'s brain. You orchestrate every response. Call think to reason, reply to respond, memory tools to remember, and done when finished. Every turn must include reply and done.`

	// Safety caps — hard limits regardless of config, to prevent runaway loops.
	maxIterationsPerWindowCap = 50
	maxContinuationsCap       = 10
)

// loadAgentPrompt reads the agent prompt from disk (hot-reloadable),
// falling back to a minimal default if the file doesn't exist.
// After reading, it replaces tool inventory markers with auto-generated
// sections from the YAML registry, then expands {{her}}/{{user}} placeholders.
// This is the same pattern as prompt.md — edit the file, restart the
// bot (or it reloads on next message), and the behavior changes.
func loadAgentPrompt(path string, cfg *config.Config) string {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		log.Warn("couldn't load agent prompt, using default", "path", path)
		return cfg.ExpandPrompt(defaultAgentPrompt)
	}
	content := expandToolSections(string(data))
	return cfg.ExpandPrompt(content)
}

// expandToolSections replaces content between marker comments in the
// agent prompt with auto-generated tool inventory from the YAML registry.
// Markers look like <!-- BEGIN HOT_TOOLS --> ... <!-- END HOT_TOOLS -->.
// If markers are missing, the content is returned unchanged.
func expandToolSections(content string) string {
	content = replaceBetweenMarkers(content, "HOT_TOOLS", tools.RenderHotToolsList("main"))
	content = replaceBetweenMarkers(content, "CATEGORY_TABLE", tools.RenderCategoryTable())
	return content
}

// replaceBetweenMarkers replaces the text between <!-- BEGIN tag --> and
// <!-- END tag --> with the replacement string. The markers themselves
// are preserved; only the content between them is swapped.
func replaceBetweenMarkers(content, tag, replacement string) string {
	begin := "<!-- BEGIN " + tag + " -->"
	end := "<!-- END " + tag + " -->"
	startIdx := strings.Index(content, begin)
	endIdx := strings.Index(content, end)
	if startIdx < 0 || endIdx < 0 || endIdx <= startIdx {
		return content // markers not found, return unchanged
	}
	return content[:startIdx+len(begin)] + "\n" + replacement + "\n" + content[endIdx:]
}

// RunParams bundles all the parameters for an agent run.
// This replaces the old 12+ argument function signature with a single
// struct, making it much easier to add new parameters without breaking
// every caller.
//
// In Python you might use **kwargs or a dataclass. In Go, a params struct
// is the idiomatic way to handle functions with many inputs.
type RunParams struct {
	DriverLLM                 *llm.Client
	MemoryAgentLLM            *llm.Client // post-turn background memory agent — nil if not configured
	ChatLLM                   *llm.Client
	VisionLLM                 *llm.Client // vision language model — nil if not configured
	ClassifierLLM             *llm.Client // classifier for memory writes — nil if not configured
	Store                     memory.Store
	EmbedClient               *embed.Client
	SimilarityThreshold       float64
	TavilyClient              *search.TavilyClient
	CalendarBridge            calendar.Bridge // nil in prod (tools create CLIBridge), FakeBridge in sims
	Cfg                       *config.Config
	ScrubbedUserMessage       string
	ScrubVault                *scrub.Vault
	ConversationID            string
	TriggerMsgID              int64
	ScheduleID                int64 // scheduler_tasks.id that triggered this run (0 if not from a schedule)
	StatusCallback            tools.StatusCallback
	SendCallback              tools.SendCallback
	TTSCallback               tools.TTSCallback           // DEPRECATED: use OnMessageSend
	OnMessageSend             tools.MessageSendCallback   // fires after each reply delivery — replaces TTSCallback
	TraceCallback             tools.TraceCallback             // nil if traces disabled
	MemoryTraceCallback       tools.TraceCallback             // nil if memory agent tracing disabled; separate message from TraceCallback
	StageResetCallback        tools.StageResetCallback        // nil-safe — sends new placeholder after reply
	DeletePlaceholderCallback tools.DeletePlaceholderCallback // nil-safe — deletes orphan placeholder on exit
	SendConfirmCallback       tools.SendConfirmCallback       // nil-safe — confirmation buttons for destructive actions
	StreamCallback            tools.StreamCallback            // nil-safe — streams chat tokens to Telegram for live typing effect
	SendPaginatedCallback     tools.SendPaginatedCallback     // nil-safe — splits long messages into pages with ◀/▶ buttons
	ImageBase64               string   // base64-encoded image data (empty if no image)
	ImageMIME                 string   // MIME type of the image (e.g., "image/jpeg")
	OCRText                   string   // pre-flight OCR text extracted from the photo (empty if no image or OCR unavailable)
	EventBus                  *tui.Bus                    // nil-safe — emits rich typed events for the TUI
	ConfigPath                string                      // path to config.yaml — needed for persisting location changes via set_location
	AgentEventCB              tools.AgentEventCallback    // nil-safe — fires when memory agent calls notify_agent
	Tracker                   *turn.Tracker               // nil-safe — manages turn lifecycle, typing, sub-agent coordination
	LiteToolHook              func(toolName string)       // nil-safe — called after each tool execution in lite trace mode
	IsSimRun                  bool                        // true when running via the sim adapter
	ReportsDir                string                      // absolute path to reports/ — file tools enforce this boundary
	WorkerCallback            func(taskType, note string, triggerMsgID int64)   // nil-safe — fires worker agent in background goroutine
	WorkerCallbackSync        func(taskType, note string, triggerMsgID int64) string // nil-safe — runs worker synchronously, returns summary
	GmailBridge               gmail.Bridge                // nil-safe — email access (APIBridge in prod, FakeBridge in sims)
}

// RunResult holds the outcome of an agent run — the reply text plus
// metrics that the bot needs for the TUI. Adding fields here is cheap
// (it's just a struct return), and avoids the bot having to query the
// DB or re-derive data the agent already has in memory.
type RunResult struct {
	ReplyText        string
	ThinkTraces      []string // driver agent's think() calls — used by introspection agent
	ToolSequence     []string // ordered tool names called this turn (e.g. ["think", "recall_memories", "reply", "done"])
	TotalCost        float64  // accumulated cost across all LLM calls (agent + chat)
	ToolCalls        int      // number of tool calls the agent made
	MemoriesSaved    int      // number of memories saved/updated during this turn
	PendingNarration   string // cleaned report text queued by narrate_report tool — bot sends as voice memo
	PublishedReportURL string // Telegraph URL from publish_report — bot auto-appends as clickable link
}

// Run executes the agent loop for one conversation turn.
// This is the core orchestration — the agent decides what tools to call
// (search, read, book lookup, memory ops) and MUST call reply exactly once
// to generate the user-facing response.
//
// Unlike the old architecture where this ran in a background goroutine,
// Run now executes SYNCHRONOUSLY because it IS the response pipeline.
// The persona evolution triggers at the end still run in a goroutine
// since they don't affect the user's response.
func Run(params RunParams) (*RunResult, error) {
	log.Info("─── agent ───")

	// Begin the main phase — the Tracker ref-counts active phases and
	// emits TurnEndEvent when all of them (main, memory, mood) finish.
	// The defer ensures Done fires even on early error returns; the
	// explicit Done() later with real metrics takes precedence (once-guarded).
	var mainPhase *turn.PhaseHandle
	if params.Tracker != nil {
		mainPhase = params.Tracker.Begin("main")
		defer mainPhase.Done(turn.PhaseMetrics{})
	}

	// Helper for nil-safe event emission — avoids if-checks everywhere.
	emit := func(e tui.Event) {
		if params.EventBus != nil {
			params.EventBus.Emit(e)
		}
	}

	// --- Compaction ---
	// Load a wider window (40 messages) so MaybeCompact can actually
	// see enough history to trigger. Previously we fed it only the
	// recent_messages window (10), which was always under the token
	// threshold — so compaction never fired and older messages just
	// vanished from context with no summary.
	const compactionWindow = 100
	compactionMsgs, err := params.Store.RecentMessages(params.ConversationID, compactionWindow)
	if err != nil {
		log.Error("loading compaction history", "err", err)
	}
	// Strip the current message to avoid duplication (it's shown
	// separately under "Current Message" in the agent context).
	if params.TriggerMsgID > 0 && len(compactionMsgs) > 0 {
		filtered := make([]memory.Message, 0, len(compactionMsgs))
		for _, msg := range compactionMsgs {
			if msg.ID != params.TriggerMsgID {
				filtered = append(filtered, msg)
			}
		}
		compactionMsgs = filtered
	}

	// Run compaction on the wider window. MaybeCompact summarizes
	// older messages and returns only the messages that should stay
	// in full fidelity. The summary gets injected into the system
	// prompt by buildChatSystemPrompt.
	var conversationSummary string
	var keptMessages []memory.Message
	if len(compactionMsgs) > 0 {
		emit(tui.CompactStartEvent{Time: time.Now(), Stream: "chat"})
		cr, compactErr := compact.MaybeCompact(
			params.ChatLLM, params.Store, params.ConversationID,
			compactionMsgs, params.Cfg.Memory.MaxHistoryTokens,
			params.Cfg.Identity.Her, params.Cfg.Identity.User,
		)
		if compactErr != nil {
			log.Error("compaction error", "err", compactErr)
			keptMessages = compactionMsgs
		} else {
			conversationSummary = cr.Summary
			keptMessages = cr.KeptMessages

			// Surface compaction in traces and TUI so it's not invisible.
			if cr.DidCompact {
				if params.TraceCallback != nil {
					params.TraceCallback(fmt.Sprintf(
						"📦 <i>compacted %d messages (%d→%d tokens)</i>",
						cr.Summarized, cr.TokensBefore, cr.TokensAfter))
				}
				emit(tui.CompactEvent{
					Time:         time.Now(),
					Summarized:   cr.Summarized,
					TokensBefore: cr.TokensBefore,
					TokensAfter:  cr.TokensAfter,
				})
			}
		}
	} else {
		keptMessages = compactionMsgs
	}

	// Trim to the configured sliding window for the agent context.
	// The agent only needs the last N messages for resolving references
	// like "it", "that book", etc. — the summary covers older context.
	recentMsgs := keptMessages
	if len(recentMsgs) > params.Cfg.Memory.RecentMessages {
		recentMsgs = recentMsgs[len(recentMsgs)-params.Cfg.Memory.RecentMessages:]
	}

	// --- Agent action history compaction ---
	// Load the agent's tool call history from agent_turns and compact it
	// if it's getting too large. This gives the agent persistent memory
	// of what it did in previous turns (facts saved, searches run, etc.).
	var agentActionSummary string
	var recentAgentActions []memory.AgentAction
	agentActions, err := params.Store.RecentAgentActions(params.ConversationID, 30) // last 30 messages worth
	if err != nil {
		log.Warn("failed to load agent actions", "err", err)
	} else if len(agentActions) > 0 {
		emit(tui.CompactStartEvent{Time: time.Now(), Stream: "agent"})
		acr, compactErr := compact.MaybeCompactAgent(
			params.ChatLLM, params.Store, params.ConversationID,
			agentActions, params.Cfg.Memory.DriverContextBudget,
			params.Cfg.Identity.Her,
		)
		if compactErr != nil {
			log.Error("agent compaction error", "err", compactErr)
			recentAgentActions = agentActions
		} else {
			agentActionSummary = acr.Summary
			recentAgentActions = acr.RecentActions
			if acr.DidCompact {
				log.Infof("  agent compacted %d actions (%d→%d tokens)",
					acr.Summarized, acr.TokensBefore, acr.TokensAfter)
			}
		}
	}

	// Emit context event for the TUI.
	if mainPhase != nil {
		mainPhase.Emit(tui.ContextEvent{
			Time: time.Now(), TurnID: params.TriggerMsgID,
		})
	} else {
		emit(tui.ContextEvent{
			Time: time.Now(), TurnID: params.TriggerMsgID,
		})
	}

	// Build the context message for the agent using the layer registry.
	// Each layer (time, history, message, image, facts) lives in its own
	// file under agent/layers/ and registers itself via init().
	layerCtx := &layers.LayerContext{
		Store:               params.Store,
		Cfg:                 params.Cfg,
		EmbedClient:         params.EmbedClient,
		ConversationSummary: conversationSummary,
		AgentActionSummary:  agentActionSummary,
		RecentAgentActions:  recentAgentActions,
		ConversationID:      params.ConversationID,
		ScrubbedUserMessage: params.ScrubbedUserMessage,
		RecentMessages:      recentMsgs,
		HasImage:            params.ImageBase64 != "",
		OCRText:             params.OCRText,
	}
	context, agentLayerResults := layers.BuildAll(layers.StreamAgent, layerCtx)

	// Log the agent context shape for observability.
	var agentTotalTokens int
	for _, lr := range agentLayerResults {
		agentTotalTokens += lr.Tokens
		if lr.Detail != "" {
			log.Infof("  [agent layer] %s: ~%d tokens (%s)", lr.Name, lr.Tokens, lr.Detail)
		} else {
			log.Infof("  [agent layer] %s: ~%d tokens", lr.Name, lr.Tokens)
		}
	}
	log.Infof("  agent context total: ~%d tokens", agentTotalTokens)

	// Load the agent prompt from disk (hot-reloadable, like prompt.md).
	agentPrompt := loadAgentPrompt(params.Cfg.Persona.AgentPromptFile, params.Cfg)

	// When direct reply mode is active, append the direct reply prompt
	// section from disk. Follows data primacy: prompt text in .md, not Go.
	if params.Cfg.Driver.DirectReply {
		if drp, err := os.ReadFile("direct_reply_prompt.md"); err == nil {
			agentPrompt += "\n\n" + params.Cfg.ExpandPrompt(string(drp))
		} else {
			log.Warn("direct reply enabled but prompt file missing", "err", err)
		}
	}

	// Set up the conversation with the agent model.
	messages := []llm.ChatMessage{
		{Role: "system", Content: agentPrompt},
		{Role: "user", Content: context},
	}

	// Start with only the hot tools (7 instead of 26). The agent can
	// load deferred tools on demand via use_tools(["search"]) etc.
	// This reduces context pressure on the agent model significantly.
	toolDefs := tools.HotToolDefs("main", params.Cfg)

	// Build the tool context with everything the tools need.
	tctx := &tools.Context{
		AgentName:                 "main",
		Store:                     params.Store,
		EmbedClient:               params.EmbedClient,
		SimilarityThreshold:       params.SimilarityThreshold,
		MaxMemoryLength:           params.Cfg.Memory.MaxMemoryLength,
		PersonaFile:               params.Cfg.Persona.PersonaFile,
		StatusCallback:            params.StatusCallback,
		SendCallback:              params.SendCallback,
		TTSCallback:               params.TTSCallback,
		OnMessageSend:             params.OnMessageSend,
		TraceCallback:             params.TraceCallback,
		StageResetCallback:        params.StageResetCallback,
		DeletePlaceholderCallback: params.DeletePlaceholderCallback,
		SendConfirmCallback:       params.SendConfirmCallback,
		StreamCallback:            params.StreamCallback,
		SendPaginatedCallback:     params.SendPaginatedCallback,
		ChatLLM:                   params.ChatLLM,
		VisionLLM:                 params.VisionLLM,
		ClassifierLLM:             params.ClassifierLLM,
		TavilyClient:              params.TavilyClient,
		CalendarBridge:            params.CalendarBridge,
		Cfg:                       params.Cfg,
		ScrubVault:                params.ScrubVault,
		ScrubbedUserMessage:       params.ScrubbedUserMessage,
		ConversationID:            params.ConversationID,
		TriggerMsgID:              params.TriggerMsgID,
		ScheduleID:                params.ScheduleID,
		ConversationSummary:       conversationSummary,
		ImageBase64:               params.ImageBase64,
		ImageMIME:                 params.ImageMIME,
		OCRText:                   params.OCRText,
		ActiveTools:               &toolDefs,
		EventBus:                  params.EventBus,
		ConfigPath:                params.ConfigPath,
		ReportsDir:                params.ReportsDir,
		WorkerCallback:            params.WorkerCallback,
		WorkerCallbackSync:       params.WorkerCallbackSync,
		GmailBridge:              params.GmailBridge,
		IsSimRun:                  params.IsSimRun,
		PreApprovedRewrites:       make(map[string]bool),
	}

	// Wire turn tracker callbacks so tools can participate in the turn
	// lifecycle without importing agent or bot packages.
	if params.Tracker != nil {
		tctx.StopTypingFn = params.Tracker.StopTyping
		tctx.Phase = mainPhase
	}

	// --- Driver-specific state ---
	turnIndex := 0
	var lastThinkContent string
	var repeatCount int
	var agentFinalText string
	var thinkTraces []string
	var toolSeq []string

	// ToolChoiceFirst — conditional on config.
	var toolChoiceFirst interface{}
	requireToolChoice := true
	if params.Cfg.Driver.RequireToolChoice != nil {
		requireToolChoice = *params.Cfg.Driver.RequireToolChoice
	}
	if requireToolChoice {
		toolChoiceFirst = "required"
	}

	// Run the tool-calling loop via the shared engine.
	loopResult, err := engine.RunLoop(engine.EngineConfig{
		Name:                "driver",
		MetricRole:          memory.RoleDriver,
		LLM:                 params.DriverLLM,
		Store:               params.Store,
		ToolDefs:            toolDefs,
		ToolCtx:             tctx,
		TriggerMsgID:        params.TriggerMsgID,
		IterationsPerWindow: params.Cfg.Driver.IterationsPerWindow,
		MaxContinuations:    params.Cfg.Driver.MaxContinuations,
		TraceCallback:       params.TraceCallback,
		LiteToolHook:        params.LiteToolHook,
		EventBus:            params.EventBus,
		Phase:               mainPhase,
		Messages:            messages,
		ToolChoiceFirst:     toolChoiceFirst,

		// Driver continuation message urges reply before continuing.
		ContinuationMsg: func(window, maxWindows int, summary string) string {
			return fmt.Sprintf(
				"You have used all iterations in the previous window without calling done. "+
					"Continuation window %d of %d. Your progress so far:\n%s\n\n"+
					"IMPORTANT: Call reply immediately to update the user on your progress, "+
					"then continue your work and call done when finished.",
				window, maxWindows, summary,
			)
		},

		// PostIteration: loop detection — catch repeated identical think calls.
		PostIteration: func(iteration, window int, resp *llm.ChatResponse) bool {
			if len(resp.ToolCalls) == 1 && resp.ToolCalls[0].Function.Name == "think" {
				if resp.ToolCalls[0].Function.Arguments == lastThinkContent {
					repeatCount++
					if repeatCount >= 2 {
						log.Warn("think loop detected, forcing exit", "repeats", repeatCount+1)
						return true
					}
				} else {
					lastThinkContent = resp.ToolCalls[0].Function.Arguments
					repeatCount = 0
				}
			} else {
				lastThinkContent = ""
				repeatCount = 0
			}
			return false
		},

		// OnNoToolCalls: detect "done" typed as text, capture fallback text.
		OnNoToolCalls: func(resp *llm.ChatResponse) bool {
			if resp.Content != "" {
				trimmed := strings.TrimSpace(strings.ToLower(resp.Content))
				if trimmed == "done" || trimmed == "done." {
					log.Info("  agent typed 'done' as text (treating as done signal)")
					tctx.DoneCalled = true
					return false // let the engine break normally via DoneCalled
				}
				log.Warnf("  agent returned text instead of tool calls: %s",
					engine.TruncateLog(resp.Content, 200))
				agentFinalText = resp.Content
			}
			return false // let the engine break
		},

		// ActiveToolGuard: only allow tools in the current ActiveTools set.
		ActiveToolGuard: func(tc llm.ToolCall) (string, bool) {
			if tc.Function.Arguments != "" && !json.Valid([]byte(tc.Function.Arguments)) {
				return fmt.Sprintf("error: malformed JSON in arguments (likely truncated by token limit). Please retry with shorter arguments. Got: %s",
					engine.TruncateLog(tc.Function.Arguments, 100)), true
			}
			for _, t := range *tctx.ActiveTools {
				if t.Function.Name == tc.Function.Name {
					return "", false
				}
			}
			return fmt.Sprintf("error: tool '%s' is not available. Use use_tools to load additional categories if needed.", tc.Function.Name), true
		},

		// PostTool: SaveAgentTurn, think trace capture, tool sequence, reply fallback annotation.
		PostTool: func(tc llm.ToolCall, result string, isError bool) {
			// Save agent turn log (assistant call).
			params.Store.SaveAgentTurn(params.TriggerMsgID, turnIndex, "assistant", tc.Function.Name, tc.Function.Arguments, "")
			turnIndex++

			// Capture think() content for the memory agent.
			if tc.Function.Name == "think" {
				var thinkArgs struct {
					Thought string `json:"thought"`
				}
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &thinkArgs); err == nil && thinkArgs.Thought != "" {
					thinkTraces = append(thinkTraces, thinkArgs.Thought)
					tctx.ThinkTraces = thinkTraces
				}
			}

			toolSeq = append(toolSeq, tc.Function.Name)

			// Save agent turn log (tool result).
			params.Store.SaveAgentTurn(params.TriggerMsgID, turnIndex, "tool", tc.Function.Name, "", result)
			turnIndex++
		},

		// OnLoopExit: auto-done, fallback reply, orphan placeholder cleanup.
		OnLoopExit: func(reason string, msgs []llm.ChatMessage) {
			// Auto-done for diffusion models that omit the done call.
			if tctx.ReplyCount > 0 && !tctx.DoneCalled {
				log.Info("auto-done: reply was called but done was not — treating as complete")
				tctx.DoneCalled = true
			}

			// Fallback: ensure the user always gets a response.
			if tctx.ReplyCount == 0 {
				if agentFinalText != "" {
					log.Warn("reply was never called, using agent text as instruction")
					if params.LiteToolHook != nil {
						params.LiteToolHook("reply")
					}
					instruction := fmt.Sprintf(`{"instruction":%s}`, mustJSON(agentFinalText))
					toolSeq = append(toolSeq, "reply")
					tools.Execute("reply", instruction, tctx)
				} else {
					log.Warn("reply was never called, generating generic fallback")
					if params.LiteToolHook != nil {
						params.LiteToolHook("reply")
					}
					toolSeq = append(toolSeq, "reply")
					tools.Execute("reply", `{"instruction":"The driver agent loop failed — NO tools were called and NO actions were taken. Do NOT claim you did something. Acknowledge the user's request and offer to try again. Be honest that nothing happened."}`, tctx)
				}
			}

			// Delete orphan placeholder from the last stage reset.
			if !tctx.ReplyCalled && tctx.ReplyCount > 0 && tctx.DeletePlaceholderCallback != nil {
				if err := tctx.DeletePlaceholderCallback(); err != nil {
					log.Warn("cleanup: failed to delete orphan placeholder", "err", err)
				}
			}
		},
	})

	// Build result from loop output + tool context state.
	var totalCost float64
	var totalToolCalls int
	if loopResult != nil {
		totalCost = loopResult.TotalCost
		totalToolCalls = loopResult.ToolCalls
	}

	if err != nil && tctx.ReplyCount == 0 {
		return nil, fmt.Errorf("agent failed to generate a reply: %w", err)
	}

	result := &RunResult{
		ReplyText:          tctx.ReplyText,
		ThinkTraces:        thinkTraces,
		ToolSequence:       toolSeq,
		TotalCost:          totalCost + tctx.ReplyCost,
		ToolCalls:          totalToolCalls,
		MemoriesSaved:      len(tctx.SavedMemories),
		PendingNarration:   tctx.PendingNarration,
		PublishedReportURL: tctx.PublishedReportURL,
	}

	if mainPhase != nil {
		mainPhase.Done(turn.PhaseMetrics{
			Cost:          result.TotalCost,
			ToolCalls:     result.ToolCalls,
			MemoriesSaved: result.MemoriesSaved,
		})
	}

	return result, nil
}

// mustJSON marshals a string to a JSON string literal (with quotes and
// escaping). Used to safely embed agent text into a JSON object without
// risking broken JSON from quotes or newlines in the content.
func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

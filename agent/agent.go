package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"her/compact"
	"her/config"
	"her/embed"
	"her/llm"
	"her/memory"
	"her/persona"
	"her/scrub"
	"her/logger"
	"her/search"
	"her/weather"
)

// log is the package-level logger for the agent package.
var log = logger.WithPrefix("agent")

// StatusCallback is a function the bot provides so the agent can update
// the Telegram message in real time. When the agent calls web_search,
// the callback edits the placeholder message to show a status like
// "searching...". When reply is called, it edits the message to the
// final response.
//
// This is the same pattern as SendMessageFunc before, but used for
// live status updates instead of follow-up messages. In Python you'd
// pass a lambda; in Go you declare the function signature as a type.
type StatusCallback func(status string) error

// SendCallback is a function the bot provides for sending NEW messages
// (as opposed to editing the placeholder). Used by the reply tool for
// follow-up replies — the first reply edits the placeholder via
// StatusCallback, subsequent replies send new messages via SendCallback.
// This lets Mira say "let me look that up" and then "here's what I found"
// as separate visible messages.
type SendCallback func(text string) error

// TTSCallback is a function the bot provides so the agent can trigger
// voice synthesis immediately when a reply is sent, rather than waiting
// for the entire agent loop to finish. This runs in a goroutine so it
// doesn't block the agent from continuing to think/act.
type TTSCallback func(text string)

// TraceCallback is a function the bot provides for sending/updating the
// agent thinking trace message. The first call sends a new message; subsequent
// calls edit it with the accumulated trace. The agent builds up trace lines
// as it processes each tool call and sends updates after each step.
// Returns the message (for subsequent edits) and any error.
type TraceCallback func(text string) error

// toolContext bundles all the dependencies that tool execution functions need.
// This grew from the original version — it now includes everything the
// reply tool needs to generate a full conversational response, plus the
// search clients for web_search, web_read, and book_search.
type toolContext struct {
	store               *memory.Store
	embedClient         *embed.Client
	similarityThreshold float64
	personaFile         string
	statusCallback      StatusCallback
	sendCallback        SendCallback
	ttsCallback         TTSCallback
	traceCallback       TraceCallback

	// chatLLM is the conversational model (Deepseek). The reply tool
	// uses this to generate the actual natural language response.
	chatLLM *llm.Client

	// visionLLM is the vision language model (Gemini Flash). The
	// view_image tool uses this to describe photos the user sends.
	// Nil if vision is not configured.
	visionLLM *llm.Client

	// imageBase64 and imageMIME hold the current photo data (if any).
	// Populated by the bot when the user sends a photo on Telegram.
	imageBase64 string
	imageMIME   string

	// tavilyClient provides web search and URL extraction.
	// Can be nil if Tavily is not configured — search tools will
	// return an error message instead of crashing.
	tavilyClient *search.TavilyClient

	// weatherClient fetches and caches current weather from Open-Meteo.
	// Used by buildWeatherContext to inject weather into the system prompt.
	// Nil if weather is not configured (no lat/lon).
	weatherClient *weather.Client

	// cfg holds the full config for building prompts (prompt file paths,
	// memory limits, etc.).
	cfg *config.Config

	// scrubVault holds the PII token mappings from the current message.
	// The reply tool uses this to deanonymize the LLM response before
	// sending it to Telegram.
	scrubVault *scrub.Vault

	// scrubbedUserMessage is the PII-scrubbed version of what the user said.
	// Used by the reply tool when building the prompt for the conversational model.
	scrubbedUserMessage string

	// conversationID identifies the current conversation for history retrieval.
	conversationID string

	// triggerMsgID is the DB message ID of the user's message that started
	// this agent run. Used for linking metrics and saving the response.
	triggerMsgID int64

	// conversationSummary is the compacted summary of older messages.
	// Injected into the system prompt so the model has context of what
	// was discussed earlier without needing the full message history.
	conversationSummary string

	// relevantFacts holds the results of semantic search on the user's message.
	// These are the facts closest in meaning to what the user just said.
	// Passed to BuildMemoryContext so the system prompt includes relevant context.
	relevantFacts []memory.Fact

	// searchContext accumulates search results, book data, and URL content
	// across tool calls. When reply is called, this context is included
	// in the prompt so the conversational model can reference it.
	searchContext string

	// replyCalled tracks whether the reply tool has been called during
	// this agent run. We check this after the loop to ensure the user
	// always gets a response.
	replyCalled bool

	// doneCalled tracks whether the done tool has been called,
	// signaling the agent is finished with all actions for this turn.
	doneCalled bool

	// replyText stores the final response text (after deanonymization).
	// Used by the bot to know what was sent.
	replyText string

	// savedFacts tracks facts saved during this agent run.
	// Used to trigger reflection (Trigger B) when enough facts accumulate.
	savedFacts []string
}

// defaultAgentPrompt is used as a fallback if agent_prompt.md can't be loaded.
const defaultAgentPrompt = `You are Mira's brain. You orchestrate every response. Call think to reason, reply to respond, memory tools to remember, and done when finished. Every turn must include reply and done.`

// loadAgentPrompt reads the agent prompt from disk (hot-reloadable),
// falling back to a minimal default if the file doesn't exist.
// This is the same pattern as prompt.md — edit the file, restart the
// bot (or it reloads on next message), and the behavior changes.
func loadAgentPrompt(path string) string {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		log.Warn("couldn't load agent prompt, using default", "path", path)
		return defaultAgentPrompt
	}
	return string(data)
}

// RunParams bundles all the parameters for an agent run.
// This replaces the old 12+ argument function signature with a single
// struct, making it much easier to add new parameters without breaking
// every caller.
//
// In Python you might use **kwargs or a dataclass. In Go, a params struct
// is the idiomatic way to handle functions with many inputs.
type RunParams struct {
	AgentLLM            *llm.Client
	ChatLLM             *llm.Client
	VisionLLM           *llm.Client // vision language model — nil if not configured
	Store               *memory.Store
	EmbedClient         *embed.Client
	SimilarityThreshold float64
	TavilyClient        *search.TavilyClient
	WeatherClient       *weather.Client
	Cfg                 *config.Config
	ScrubbedUserMessage string
	ScrubVault          *scrub.Vault
	ConversationID      string
	TriggerMsgID        int64
	StatusCallback      StatusCallback
	SendCallback        SendCallback
	TTSCallback         TTSCallback
	TraceCallback       TraceCallback // nil if traces disabled
	ReflectionThreshold int
	RewriteEveryN       int
	ImageBase64         string // base64-encoded image data (empty if no image)
	ImageMIME           string // MIME type of the image (e.g., "image/jpeg")
}

// RunResult holds the outcome of an agent run — primarily the reply
// text that was sent to the user, so the bot can use it for logging.
type RunResult struct {
	ReplyText string
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

	// Gather current facts for the agent's context.
	facts, err := params.Store.AllActiveFacts()
	if err != nil {
		log.Error("loading facts", "err", err)
		return nil, fmt.Errorf("loading facts: %w", err)
	}

	// Split facts into user and self categories for the context.
	var userFacts, selfFacts []memory.Fact
	for _, f := range facts {
		if f.Subject == "self" {
			selfFacts = append(selfFacts, f)
		} else {
			userFacts = append(userFacts, f)
		}
	}
	log.Infof("  facts: %d user, %d self", len(userFacts), len(selfFacts))

	// Load recent conversation history so the agent can resolve
	// references like "it", "that book", "what we talked about", etc.
	// We exclude the current message (TriggerMsgID) from history because
	// it's already shown separately under "Current Message" in the agent
	// context. Without this filter, the agent sees the message twice and
	// thinks the user is "repeating" themselves.
	recentMsgs, err := params.Store.RecentMessages(params.ConversationID, params.Cfg.Memory.RecentMessages)
	if err != nil {
		log.Error("loading history", "err", err)
	}
	// Strip the current message from history to avoid duplication.
	if params.TriggerMsgID > 0 && len(recentMsgs) > 0 {
		filtered := make([]memory.Message, 0, len(recentMsgs))
		for _, msg := range recentMsgs {
			if msg.ID != params.TriggerMsgID {
				filtered = append(filtered, msg)
			}
		}
		recentMsgs = filtered
	}

	// Run compaction if the conversation history is getting long.
	// This summarizes older messages into a running summary, keeping
	// recent messages in full fidelity. The summary gets injected
	// into the prompt by buildChatSystemPrompt.
	var conversationSummary string
	if len(recentMsgs) > 0 {
		conversationSummary, recentMsgs, err = compact.MaybeCompact(
			params.ChatLLM, params.Store, params.ConversationID,
			recentMsgs, params.Cfg.Memory.MaxHistoryTokens,
		)
		if err != nil {
			log.Error("compaction error", "err", err)
		}
	}

	// Semantic search — find facts most relevant to what the user just said.
	// This is the core of v0.4: instead of showing the LLM ALL facts sorted
	// by importance, we embed the user's message and find the closest matches
	// via sqlite-vec KNN. The results go into the system prompt so the
	// conversational model has the right context without seeing everything.
	var relevantFacts []memory.Fact
	if params.EmbedClient != nil && params.Store.EmbedDimension > 0 {
		queryVec, err := params.EmbedClient.Embed(params.ScrubbedUserMessage)
		if err != nil {
			log.Warn("semantic search: embedding failed, falling back to importance-only", "err", err)
		} else {
			relevantFacts, err = params.Store.SemanticSearch(queryVec, params.Cfg.Memory.MaxFactsInContext)
			if err != nil {
				log.Warn("semantic search: query failed, falling back to importance-only", "err", err)
			} else {
				log.Infof("  semantic search: %d relevant facts", len(relevantFacts))
			}
		}
	}

	// Build the context message for the agent. We pass the current
	// time and timezone so the agent can convert natural language times
	// to ISO timestamps for create_reminder.
	context := buildAgentContext(params.ScrubbedUserMessage, recentMsgs, userFacts, selfFacts, params.ImageBase64 != "", params.Cfg.Scheduler.Timezone)

	// Load the agent prompt from disk (hot-reloadable, like prompt.md).
	agentPrompt := loadAgentPrompt(params.Cfg.Persona.AgentPromptFile)

	// Set up the conversation with the agent model.
	messages := []llm.ChatMessage{
		{Role: "system", Content: agentPrompt},
		{Role: "user", Content: context},
	}

	tools := ToolDefs()

	// Build the tool context with everything the tools need.
	tctx := &toolContext{
		store:               params.Store,
		embedClient:         params.EmbedClient,
		similarityThreshold: params.SimilarityThreshold,
		personaFile:         params.Cfg.Persona.PersonaFile,
		statusCallback:      params.StatusCallback,
		sendCallback:        params.SendCallback,
		ttsCallback:         params.TTSCallback,
		traceCallback:       params.TraceCallback,
		chatLLM:             params.ChatLLM,
		visionLLM:           params.VisionLLM,
		tavilyClient:        params.TavilyClient,
		weatherClient:       params.WeatherClient,
		cfg:                 params.Cfg,
		scrubVault:          params.ScrubVault,
		scrubbedUserMessage: params.ScrubbedUserMessage,
		conversationID:      params.ConversationID,
		triggerMsgID:        params.TriggerMsgID,
		conversationSummary: conversationSummary,
		relevantFacts:       relevantFacts,
		imageBase64:         params.ImageBase64,
		imageMIME:           params.ImageMIME,
	}

	// Tool-calling loop. The model may return multiple tool calls,
	// or it may return tool calls that require a follow-up turn.
	// Track turn index for agent_turns logging.
	turnIndex := 0

	// We loop up to 10 iterations to allow for think + search + refine cycles.
	// With the think tool, a typical complex flow might use 6-7 iterations:
	// think → search → think(evaluate) → search(refine) → think → reply → save_fact
	// --- Agent tool-calling loop ---
	// Modeled after Crush (charmbracelet/fantasy): loop while the model
	// keeps returning tool calls (finish_reason == "tool_calls"). When the
	// model stops calling tools (finish_reason == "stop"), the loop ends.
	// No tool_choice forcing — the model decides, and we handle gracefully
	// when it doesn't cooperate.
	//
	// Loop detection: track think content to catch the agent repeating
	// itself. Crush uses SHA-256 signatures; we keep it simpler since
	// our tool set is smaller and think loops are the main failure mode.
	var lastThinkContent string
	var repeatCount int
	// agentFinalText captures any text the agent outputs when it stops
	// calling tools. Used as fallback instruction if reply wasn't called.
	var agentFinalText string

	// --- Trace builder ---
	// Accumulates formatted trace lines as the agent executes. If tracing
	// is enabled, the trace message gets sent/updated after each tool call
	// so the user can watch the agent think in real time.
	var traceLines []string
	tracing := tctx.traceCallback != nil

	// sendTrace pushes the current trace to Telegram (sends or edits).
	sendTrace := func() {
		if !tracing || len(traceLines) == 0 {
			return
		}
		text := strings.Join(traceLines, "\n")
		if err := tctx.traceCallback(text); err != nil {
			log.Warn("trace: failed to send/update", "err", err)
		}
	}

	for i := 0; i < 10; i++ {
		resp, err := params.AgentLLM.ChatCompletionWithTools(messages, tools)
		if err != nil {
			log.Error("LLM error", "err", err)
			if tracing {
				traceLines = append(traceLines, fmt.Sprintf("❌ <b>error:</b> %s", truncateLog(err.Error(), 100)))
				sendTrace()
			}
			break
		}

		// Log agent metrics.
		params.Store.SaveMetric(resp.Model, resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens, resp.CostUSD, 0, params.TriggerMsgID)
		log.Infof("  tokens: %d prompt + %d completion | $%.6f | finish=%s",
			resp.PromptTokens, resp.CompletionTokens, resp.CostUSD, resp.FinishReason)

		// --- Check finish_reason to decide how to proceed ---
		hasToolCalls := len(resp.ToolCalls) > 0

		if !hasToolCalls {
			if resp.Content != "" {
				trimmed := strings.TrimSpace(strings.ToLower(resp.Content))
				// If the agent just typed "done" as text instead of calling
				// the done tool, treat it as a done signal. MiniMax does this.
				if trimmed == "done" || trimmed == "done." {
					log.Info("  agent typed 'done' as text (treating as done signal)")
					break
				}
				log.Infof("  agent text (no tools): %s", truncateLog(resp.Content, 200))
				agentFinalText = resp.Content
			} else {
				log.Info("  done (no actions)")
			}
			break
		}

		// --- Loop detection ---
		if len(resp.ToolCalls) == 1 && resp.ToolCalls[0].Function.Name == "think" {
			if resp.ToolCalls[0].Function.Arguments == lastThinkContent {
				repeatCount++
				if repeatCount >= 2 {
					log.Warn("think loop detected, forcing exit", "repeats", repeatCount+1)
					if tracing {
						traceLines = append(traceLines, "⚠️ <i>loop detected — forcing exit</i>")
						sendTrace()
					}
					break
				}
			} else {
				lastThinkContent = resp.ToolCalls[0].Function.Arguments
				repeatCount = 0
			}
		} else {
			lastThinkContent = ""
			repeatCount = 0
		}

		log.Infof("  %d tool call(s):", len(resp.ToolCalls))

		// Append the assistant message with tool calls to the conversation.
		messages = append(messages, llm.ChatMessage{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})

		// Execute each tool call, feed results back to the model,
		// and build trace lines for observability.
		for _, tc := range resp.ToolCalls {
			params.Store.SaveAgentTurn(params.TriggerMsgID, turnIndex, "assistant", tc.Function.Name, tc.Function.Arguments, "")
			turnIndex++

			result := executeTool(tc, tctx)
			log.Infof("    → %s: %s", tc.Function.Name, truncateLog(result, 200))

			// Build the trace line for this tool call.
			if tracing {
				traceLines = append(traceLines, formatTraceLine(tc.Function.Name, tc.Function.Arguments, result))
				sendTrace()
			}

			params.Store.SaveAgentTurn(params.TriggerMsgID, turnIndex, "tool", tc.Function.Name, "", result)
			turnIndex++

			messages = append(messages, llm.ChatMessage{
				Role:       "tool",
				Content:    result,
				ToolCallID: tc.ID,
			})
		}

		// Exit when the agent explicitly signals it's done.
		// (The "done" trace line is already added by formatTraceLine above.)
		if tctx.doneCalled {
			log.Info("  done signal received")
			break
		}

		// Also exit if finish_reason was "stop" even though tools were
		// present — some providers do this (the OpenCode #14972 bug).
		if resp.FinishReason == "stop" {
			log.Info("  finish_reason=stop after tool execution")
			break
		}
	}

	// --- Fallback: ensure the user always gets a response ---
	// If the agent never called the reply tool, we still need to respond.
	// If the agent produced text (it "talked" instead of calling tools),
	// use that as the instruction so the response is at least guided by
	// what the agent intended. Otherwise, generate a generic response.
	if !tctx.replyCalled {
		if agentFinalText != "" {
			log.Warn("reply was never called, using agent text as instruction")
			instruction := fmt.Sprintf(`{"instruction":%s}`, mustJSON(agentFinalText))
			fallbackResult := execReply(instruction, tctx)
			if !tctx.replyCalled {
				log.Error("fallback reply failed", "result", fallbackResult)
				return nil, fmt.Errorf("agent failed to generate a reply")
			}
		} else {
			log.Warn("reply was never called, generating generic fallback")
			fallbackResult := execReply(`{"instruction":"The user sent a message. Respond naturally and conversationally."}`, tctx)
			if !tctx.replyCalled {
				log.Error("fallback reply also failed", "result", fallbackResult)
				return nil, fmt.Errorf("agent failed to generate a reply")
			}
		}
	}

	result := &RunResult{
		ReplyText: tctx.replyText,
	}

	// --- Persona Evolution Triggers ---
	// These run AFTER the response has been sent to the user.
	// They go in a goroutine because they don't affect the current turn.
	//
	// The chain: facts accumulate → triggers reflection →
	//            reflections accumulate → triggers persona rewrite
	// No concept of "conversations" needed — just fact and reflection counts.
	go func() {
		// Trigger: Reflection — have enough new facts accumulated since the last reflection?
		if params.ReflectionThreshold > 0 {
			factCount, err := params.Store.FactCountSinceLastReflection()
			if err != nil {
				log.Error("checking fact count for reflection trigger", "err", err)
			} else if factCount >= params.ReflectionThreshold {
				log.Infof("  [persona] reflection triggered (%d facts, threshold: %d)", factCount, params.ReflectionThreshold)

				// Gather the recent facts for the reflection prompt.
				recentFacts, _ := params.Store.RecentFacts("user", factCount)
				var factStrings []string
				for _, f := range recentFacts {
					factStrings = append(factStrings, f.Fact)
				}

				if err := persona.Reflect(params.ChatLLM, params.Store, params.ScrubbedUserMessage, tctx.replyText, factStrings); err != nil {
					log.Error("reflection error", "err", err)
				}
			}
		}

		// Trigger: Persona rewrite — have enough reflections accumulated?
		// Rewrites fire at N, 2N, 3N, ... reflections (e.g. 3, 6, 9).
		// We check: totalReflections >= (rewrites+1) * threshold.
		// This way each rewrite "consumes" a batch and won't re-trigger
		// until the next batch accumulates.
		if params.RewriteEveryN > 0 {
			totalReflections, err := params.Store.TotalReflectionCount()
			if err != nil {
				log.Error("checking reflection count for rewrite trigger", "err", err)
			} else {
				rewriteCount, err := params.Store.PersonaRewriteCount()
				if err != nil {
					log.Error("checking persona rewrite count", "err", err)
				} else {
					nextThreshold := (rewriteCount + 1) * params.RewriteEveryN
					if totalReflections >= nextThreshold {
						log.Infof("  [persona] rewrite triggered (%d reflections, next threshold: %d)", totalReflections, nextThreshold)
						if rewritten, err := persona.MaybeRewrite(params.ChatLLM, params.Store, params.Cfg.Persona.PersonaFile, 0); err != nil {
							log.Error("persona rewrite error", "err", err)
						} else if rewritten {
							log.Info("persona.md rewritten")
						}
					}
				}
			}
		}
	}()

	return result, nil
}

// buildAgentContext formats the user's message, recent conversation history,
// and current facts into a context string for the agent to reason about.
//
// The conversation history is critical — without it, the agent can't resolve
// references like "it", "that book", "what you said earlier". This was the
// cause of the wrong-search-term bug where the agent searched for AI realism
// instead of The Martian's realism.
func buildAgentContext(userMessage string, history []memory.Message, userFacts, selfFacts []memory.Fact, hasImage bool, timezone string) string {
	var b strings.Builder

	// Current date/time — the agent needs this to convert natural
	// language times ("in 2 hours", "tomorrow at 3pm") to absolute
	// ISO timestamps for create_reminder.
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		loc = time.UTC
	}
	now := time.Now().In(loc)
	fmt.Fprintf(&b, "## Current Time\n\n%s (timezone: %s)\n\n", now.Format("2006-01-02T15:04:05 (Monday)"), loc.String())

	// Recent conversation history — gives the agent context for references.
	if len(history) > 0 {
		b.WriteString("## Recent Conversation\n\n")
		for _, msg := range history {
			role := "User"
			if msg.Role == "assistant" {
				role = "Mira"
			}
			content := msg.ContentScrubbed
			if content == "" {
				content = msg.ContentRaw
			}
			fmt.Fprintf(&b, "**%s:** %s\n\n", role, content)
		}
	}

	b.WriteString("## Current Message\n\n")
	fmt.Fprintf(&b, "%s\n\n", userMessage)

	// If the user sent a photo, tell the agent explicitly so it knows
	// to call view_image before replying.
	if hasImage {
		b.WriteString("## Attached Image\n\n")
		b.WriteString("The user sent a photo. Call `view_image` to see what's in it before replying.\n\n")
	}

	b.WriteString("## User Memories\n\n")
	if len(userFacts) > 0 {
		for _, f := range userFacts {
			fmt.Fprintf(&b, "- [ID=%d, %s, importance=%d] %s\n", f.ID, f.Category, f.Importance, f.Fact)
		}
	} else {
		b.WriteString("(none yet)\n")
	}

	b.WriteString("\n## Self Memories (Mira's own knowledge)\n\n")
	if len(selfFacts) > 0 {
		for _, f := range selfFacts {
			fmt.Fprintf(&b, "- [ID=%d, %s, importance=%d] %s\n", f.ID, f.Category, f.Importance, f.Fact)
		}
	} else {
		b.WriteString("(none yet)\n")
	}

	b.WriteString("\nDecide what to do: search if needed, then reply, then manage memory if appropriate.")
	return b.String()
}

// executeTool runs a single tool call and returns a result string.
// If the tool call has truncated/malformed JSON arguments (usually from
// hitting max_tokens mid-generation), we return an error message that
// tells the model what happened so it can retry with shorter arguments.
func executeTool(tc llm.ToolCall, tctx *toolContext) string {
	// Validate JSON before dispatching. Truncated tool calls happen when
	// the model hits max_tokens while generating the arguments JSON.
	// Rather than letting each tool fail with a confusing parse error,
	// give the model clear feedback so it can self-correct.
	if tc.Function.Arguments != "" && !json.Valid([]byte(tc.Function.Arguments)) {
		return fmt.Sprintf("error: malformed JSON in arguments (likely truncated by token limit). Please retry with shorter arguments. Got: %s", truncateLog(tc.Function.Arguments, 100))
	}

	switch tc.Function.Name {
	case "reply":
		return execReply(tc.Function.Arguments, tctx)
	case "web_search":
		return execWebSearch(tc.Function.Arguments, tctx)
	case "web_read":
		return execWebRead(tc.Function.Arguments, tctx)
	case "book_search":
		return execBookSearch(tc.Function.Arguments, tctx)
	case "save_fact":
		return execSaveFact(tc.Function.Arguments, "user", tctx)
	case "save_self_fact":
		return execSaveFact(tc.Function.Arguments, "self", tctx)
	case "update_fact":
		return execUpdateFact(tc.Function.Arguments, tctx)
	case "remove_fact":
		return execRemoveFact(tc.Function.Arguments, tctx.store)
	case "update_persona":
		return execUpdatePersona(tc.Function.Arguments, tctx.store, tctx.personaFile)
	case "view_image":
		return execViewImage(tc.Function.Arguments, tctx)
	case "create_reminder":
		return execCreateReminder(tc.Function.Arguments, tctx)
	case "create_schedule":
		return execCreateSchedule(tc.Function.Arguments, tctx)
	case "list_schedules":
		return execListSchedules(tc.Function.Arguments, tctx)
	case "update_schedule":
		return execUpdateSchedule(tc.Function.Arguments, tctx)
	case "delete_schedule":
		return execDeleteSchedule(tc.Function.Arguments, tctx)
	case "recall_memories":
		return execRecallMemories(tc.Function.Arguments, tctx)
	case "log_mood":
		return execLogMood(tc.Function.Arguments, tctx)
	case "think":
		return execThink(tc.Function.Arguments, tctx)
	case "get_current_time":
		return execGetCurrentTime(tctx)
	case "set_location":
		return execSetLocation(tc.Function.Arguments, tctx)
	case "no_action":
		return "ok, no action taken"
	case "done":
		tctx.doneCalled = true
		log.Info("  done called — finishing turn")
		return "ok, turn complete"
	default:
		return fmt.Sprintf("unknown tool: %s", tc.Function.Name)
	}
}

// --- Reply tool ---

// execReply is the most important tool. It builds the full conversational
// prompt (prompt.md + persona + memory + search context + history) and
// calls the chatLLM to generate the actual response the user sees.
func execReply(argsJSON string, tctx *toolContext) string {
	var args struct {
		Instruction string `json:"instruction"`
		Context     string `json:"context"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	// Build the system prompt — same layered approach as the old buildSystemPrompt
	// in the bot package, but done here because the agent now owns the pipeline.
	systemPrompt := buildChatSystemPrompt(tctx)

	// Combine any accumulated search context with the explicit context parameter.
	fullContext := tctx.searchContext
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
	recentMsgs, err := tctx.store.RecentMessages(tctx.conversationID, tctx.cfg.Memory.RecentMessages)
	if err != nil {
		log.Error("reply: loading history", "err", err)
	} else {
		for _, msg := range recentMsgs {
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
		Content: tctx.scrubbedUserMessage,
	})

	// Call the conversational model.
	start := time.Now()
	resp, err := tctx.chatLLM.ChatCompletion(llmMessages)
	latencyMs := int(time.Since(start).Milliseconds())

	if err != nil {
		log.Error("reply: LLM error", "err", err)
		return fmt.Sprintf("error generating response: %v", err)
	}

	log.Infof("  reply: %d prompt + %d completion = %d total | $%.6f | %dms",
		resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens, resp.CostUSD, latencyMs)

	// Guard against degenerate responses. If the chat model returned
	// something suspiciously short (< 5 chars) or repetitive, it was
	// likely rate-limited or glitching. These garbage responses poison
	// the conversation history if saved, causing a feedback loop where
	// every subsequent turn degenerates further (the "ohohoh" incident).
	if isDegenerate(resp.Content) {
		log.Warn("reply: degenerate response detected, retrying once", "content", truncateLog(resp.Content, 80))
		// One retry — if the model is genuinely down, the fallback
		// in the agent loop will catch it.
		resp, err = tctx.chatLLM.ChatCompletion(llmMessages)
		if err != nil {
			log.Error("reply: retry LLM error", "err", err)
			return fmt.Sprintf("error generating response: %v", err)
		}
		if isDegenerate(resp.Content) {
			log.Error("reply: degenerate response on retry too", "content", truncateLog(resp.Content, 80))
			return "error: model returned a degenerate response. Try again in a moment."
		}
	}

	// Save the response to the database.
	respID, err := tctx.store.SaveMessage("assistant", resp.Content, resp.Content, tctx.conversationID)
	if err != nil {
		log.Error("reply: saving response", "err", err)
	}

	// Update token counts on both the user message and the response.
	if tctx.triggerMsgID > 0 {
		tctx.store.UpdateMessageTokenCount(tctx.triggerMsgID, resp.PromptTokens)
	}
	if respID > 0 {
		tctx.store.UpdateMessageTokenCount(respID, resp.CompletionTokens)
		tctx.store.SaveMetric(resp.Model, resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens, resp.CostUSD, latencyMs, respID)
	}

	// Deanonymize PII tokens before sending to the user.
	// The LLM might have used placeholders like [PHONE_1] in its response —
	// we swap those back to the real values before the user sees it.
	replyText := scrub.Deanonymize(resp.Content, tctx.scrubVault)

	// Deliver the response to Telegram.
	// First reply: edit the placeholder message (statusCallback).
	// Follow-up replies: send as a new message (sendCallback) so both
	// are visible — e.g., "let me look that up" → "here's what I found".
	if tctx.replyCalled && tctx.sendCallback != nil {
		// Follow-up reply — send as a new message.
		if err := tctx.sendCallback(replyText); err != nil {
			log.Error("reply: sending follow-up to Telegram", "err", err)
		}
	} else if tctx.statusCallback != nil {
		// First reply — edit the placeholder.
		if err := tctx.statusCallback(replyText); err != nil {
			log.Error("reply: sending to Telegram", "err", err)
		}
	}

	// Fire TTS immediately — don't wait for the agent loop to finish.
	// This runs in a goroutine so the agent can keep thinking/acting
	// while the voice memo is being synthesized and sent.
	if tctx.ttsCallback != nil {
		go tctx.ttsCallback(replyText)
	}

	tctx.replyCalled = true
	tctx.replyText = replyText

	return fmt.Sprintf("reply sent (%d chars)", len(replyText))
}


// isDegenerate detects garbage LLM outputs that would poison conversation
// history if saved. Catches single-character responses, excessive repetition
// (like "ohohohohoh..."), and empty responses. These typically happen when
// the model is rate-limited, overloaded, or in a degenerate loop.
func isDegenerate(text string) bool {
	trimmed := strings.TrimSpace(text)

	// Empty or extremely short (single char, stray punctuation).
	if len(trimmed) < 3 {
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

// buildChatSystemPrompt assembles the full system prompt for the
// conversational model, exactly as the old bot.buildSystemPrompt did.
func buildChatSystemPrompt(tctx *toolContext) string {
	var parts []string

	// Layer 1: prompt.md — base identity (hot-reloaded from disk).
	if promptBytes, err := os.ReadFile(tctx.cfg.Persona.PromptFile); err == nil {
		parts = append(parts, string(promptBytes))
	}

	// Layer 2: persona.md — evolving self-image (if it exists).
	if personaBytes, err := os.ReadFile(tctx.cfg.Persona.PersonaFile); err == nil {
		parts = append(parts, string(personaBytes))
	}

	// Layer 3: Current time — always injected so Mira knows what time
	// of day it is, what day of the week, etc. This is NOT optional —
	// without it, she has no sense of time at all.
	parts = append(parts, buildTimeContext(tctx.cfg.Scheduler.Timezone))

	// Layer 4: Memory context — blend of semantically relevant facts
	// (from KNN search) and high-importance facts (always-present).
	if memCtx, err := memory.BuildMemoryContext(tctx.store, tctx.cfg.Memory.MaxFactsInContext, tctx.relevantFacts); err == nil && memCtx != "" {
		parts = append(parts, memCtx)
	}

	// Layer 4: Weather context — current conditions so Mira can reference
	// the weather naturally. Only included if weather is configured.
	if weatherCtx := buildWeatherContext(tctx.weatherClient); weatherCtx != "" {
		parts = append(parts, weatherCtx)
	}

	// Layer 5: Mood context — recent mood trend so Mira is aware of
	// emotional patterns. Only included if there's mood data.
	if moodCtx := buildMoodContext(tctx.store); moodCtx != "" {
		parts = append(parts, moodCtx)
	}

	// Layer 5: Conversation summary — compacted older messages.
	// This gives the model awareness of what was discussed earlier
	// without burning tokens on the full message history.
	if tctx.conversationSummary != "" {
		parts = append(parts, fmt.Sprintf("# Earlier in This Conversation\n\n%s", tctx.conversationSummary))
	}

	return strings.Join(parts, "\n\n---\n\n")
}

// --- Reasoning tool ---

// execThink is the agent's "pause and think" tool. It does nothing
// except log the thought and return "ok" — but it gives the agent a
// structured place to reason before deciding what to do next.
//
// This is a common pattern in agentic systems. Without it, the model
// often skips reasoning and jumps straight to tool calls. With it,
// you get traces like:
//   think("search results are about AI, not The Martian — need to refine")
//   web_search("The Martian Andy Weir scientific accuracy")
//   think("these results are much better, user will want to know about...")
//   reply(...)
// execSetLocation looks up a city name via Open-Meteo geocoding and
// updates the weather client's coordinates. Also saves the location
// as a fact so it persists across restarts.
func execSetLocation(argsJSON string, tctx *toolContext) string {
	var args struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}
	if args.Query == "" {
		return "error: query is required (e.g., 'Portland Oregon')"
	}

	// Look up coordinates from the city name.
	loc, err := weather.GeocodeLookup(args.Query)
	if err != nil {
		return fmt.Sprintf("error: couldn't find location for %q: %v", args.Query, err)
	}

	// Update the weather client so future weather fetches use the new location.
	if tctx.weatherClient != nil {
		tctx.weatherClient.SetLocation(loc.Latitude, loc.Longitude)
	}

	// Save as a fact so the location persists across restarts.
	locationFact := fmt.Sprintf("User is located in %s, %s, %s (%.4f, %.4f)",
		loc.Name, loc.Region, loc.Country, loc.Latitude, loc.Longitude)
	_, _ = tctx.store.SaveFact(locationFact, "location", "user", tctx.triggerMsgID, 9, nil)

	log.Info("set_location", "query", args.Query, "result", locationFact)

	return fmt.Sprintf("Location set to %s, %s, %s. Weather data will now reflect this location.",
		loc.Name, loc.Region, loc.Country)
}

// execGetCurrentTime returns the current date and time in the user's
// configured timezone. Simple but essential — without this, the agent
// has no idea if it's morning or midnight.
func execGetCurrentTime(tctx *toolContext) string {
	loc, err := time.LoadLocation(tctx.cfg.Scheduler.Timezone)
	if err != nil {
		loc = time.UTC
	}

	now := time.Now().In(loc)

	// Include day of week, full date, time, and timezone — everything
	// the agent might need for time-based reasoning.
	result := now.Format("Monday, January 2, 2006 at 3:04 PM (MST)")
	log.Info("  get_current_time", "result", result)
	return result
}

func execThink(argsJSON string, tctx *toolContext) string {
	var args struct {
		Thought string `json:"thought"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "ok"
	}

	log.Infof("  think: %s", args.Thought)
	return "ok"
}

// execRecallMemories searches stored facts by semantic similarity.
// The agent calls this when it needs to actively look something up
// in memory — "do you remember when I told you about..." style queries.
func execRecallMemories(argsJSON string, tctx *toolContext) string {
	var args struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	if tctx.embedClient == nil {
		return "memory search is not available (embedding client not configured)"
	}
	if tctx.store.EmbedDimension == 0 {
		return "memory search is not available (vector index not configured)"
	}

	if args.Limit <= 0 || args.Limit > 10 {
		args.Limit = 5
	}

	// Embed the query and search.
	queryVec, err := tctx.embedClient.Embed(args.Query)
	if err != nil {
		return fmt.Sprintf("error embedding query: %v", err)
	}

	facts, err := tctx.store.SemanticSearch(queryVec, args.Limit)
	if err != nil {
		return fmt.Sprintf("error searching memories: %v", err)
	}

	if len(facts) == 0 {
		return "no matching memories found"
	}

	// Format results for the agent. Include distance so it can judge relevance.
	// Cosine distance: 0 = identical, 1 = orthogonal, 2 = opposite.
	var b strings.Builder
	fmt.Fprintf(&b, "Found %d matching memories:\n\n", len(facts))
	for _, f := range facts {
		similarity := 1 - f.Distance // convert distance to similarity for readability
		fmt.Fprintf(&b, "- [ID=%d, %s, importance=%d, similarity=%.0f%%] %s\n",
			f.ID, f.Category, f.Importance, similarity*100, f.Fact)
	}

	log.Infof("  recall_memories: %d results for %q", len(facts), args.Query)
	return b.String()
}

// buildTimeContext returns the current date/time for the system prompt.
// Always included — this is how Mira knows if it's morning or midnight,
// weekday or weekend, etc. Without this, time-aware responses are impossible.
func buildTimeContext(timezone string) string {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		loc = time.UTC
	}
	now := time.Now().In(loc)
	return "# Current Time\n" + now.Format("Monday, January 2, 2006 at 3:04 PM (MST)")
}

// buildWeatherContext returns a short weather summary for the system prompt.
// Returns "" if weather is not configured or unavailable.
//
// This is "passive context" — Mira doesn't announce the weather unprompted,
// but she can weave it into conversation when relevant ("stay dry today",
// "nice day to work outside", etc.).
func buildWeatherContext(client *weather.Client) string {
	if client == nil {
		return ""
	}

	summary := client.FormatContext()
	if summary == "" {
		return ""
	}

	return "# Current Weather\n" + summary
}

// buildMoodContext formats recent mood data for the system prompt.
// Returns an empty string if no mood data exists.
func buildMoodContext(store *memory.Store) string {
	entries, err := store.RecentMoodEntries(5)
	if err != nil || len(entries) == 0 {
		return ""
	}

	labels := map[int]string{1: "bad", 2: "rough", 3: "meh", 4: "good", 5: "great"}

	var b strings.Builder
	b.WriteString("# Mood Awareness\n\n")
	b.WriteString("Recent emotional states (use this to be attentive, not to announce it):\n\n")

	for _, e := range entries {
		label := labels[e.Rating]
		if label == "" {
			label = "unknown"
		}
		ts := e.Timestamp.Format("Mon Jan 2, 3:04 PM")
		if e.Note != "" {
			fmt.Fprintf(&b, "- %s: %s (%d/5) — %s\n", ts, label, e.Rating, e.Note)
		} else {
			fmt.Fprintf(&b, "- %s: %s (%d/5)\n", ts, label, e.Rating)
		}
	}

	// Add trend summary if we have enough data.
	avg, count, err := store.MoodTrend(10)
	if err == nil && count >= 3 {
		var trend string
		switch {
		case avg >= 4.0:
			trend = "trending positive"
		case avg >= 3.0:
			trend = "mostly neutral"
		case avg >= 2.0:
			trend = "trending down"
		default:
			trend = "going through a rough patch"
		}
		fmt.Fprintf(&b, "\nOverall trend (last %d entries): %.1f/5 — %s\n", count, avg, trend)
	}

	return b.String()
}

// execLogMood saves a mood entry from the agent when the user expresses
// how they're feeling. This is the "manual" source — the agent explicitly
// decided to log mood based on what the user said.
func execLogMood(argsJSON string, tctx *toolContext) string {
	var args struct {
		Rating int    `json:"rating"`
		Note   string `json:"note"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	id, err := tctx.store.SaveMoodEntry(args.Rating, args.Note, "", "manual", tctx.conversationID)
	if err != nil {
		return fmt.Sprintf("error saving mood: %v", err)
	}

	labels := map[int]string{1: "bad", 2: "rough", 3: "meh", 4: "good", 5: "great"}
	label := labels[args.Rating]
	log.Infof("  mood logged: %d/5 (%s) — %s", args.Rating, label, args.Note)
	return fmt.Sprintf("mood logged ID=%d: %d/5 (%s) — %s", id, args.Rating, label, args.Note)
}

// --- Search tool execution ---

// execWebSearch calls Tavily to search the web and returns formatted results.
// It also updates the Telegram message with a status indicator.
func execWebSearch(argsJSON string, tctx *toolContext) string {
	var args struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	if tctx.tavilyClient == nil {
		return "web search is not configured (no Tavily API key)"
	}

	// Show a status update in Telegram.
	if tctx.statusCallback != nil {
		_ = tctx.statusCallback(fmt.Sprintf("\U0001F50D searching for: %s...", args.Query))
	}

	resp, err := tctx.tavilyClient.Search(args.Query, 5)
	if err != nil {
		log.Error("web_search failed", "err", err)
		return fmt.Sprintf("search failed: %v", err)
	}

	formatted := search.FormatSearchResults(resp)

	// Accumulate in search context so the reply tool can use it.
	if tctx.searchContext != "" {
		tctx.searchContext += "\n\n"
	}
	tctx.searchContext += fmt.Sprintf("## Web Search: %s\n\n%s", args.Query, formatted)

	// Save to DB for observability.
	tctx.store.SaveSearch(tctx.triggerMsgID, "web", args.Query, formatted, len(resp.Results))

	log.Infof("  web_search: %d results for %q", len(resp.Results), args.Query)
	return formatted
}

// execWebRead calls Tavily extract to read a specific URL.
func execWebRead(argsJSON string, tctx *toolContext) string {
	var args struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	if tctx.tavilyClient == nil {
		return "web read is not configured (no Tavily API key)"
	}

	// Show a status update in Telegram.
	if tctx.statusCallback != nil {
		_ = tctx.statusCallback(fmt.Sprintf("\U0001F4D6 reading: %s...", args.URL))
	}

	resp, err := tctx.tavilyClient.Extract([]string{args.URL})
	if err != nil {
		log.Error("web_read failed", "err", err)
		return fmt.Sprintf("failed to read URL: %v", err)
	}

	formatted := search.FormatExtractResults(resp)

	// Accumulate in search context.
	if tctx.searchContext != "" {
		tctx.searchContext += "\n\n"
	}
	tctx.searchContext += fmt.Sprintf("## Content from %s\n\n%s", args.URL, formatted)

	// Save to DB for observability.
	tctx.store.SaveSearch(tctx.triggerMsgID, "web_read", args.URL, formatted, len(resp.Results))

	log.Infof("  web_read: extracted from %s", args.URL)
	return formatted
}

// execBookSearch queries Open Library for book information.
func execBookSearch(argsJSON string, tctx *toolContext) string {
	var args struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	// Show a status update in Telegram.
	if tctx.statusCallback != nil {
		_ = tctx.statusCallback(fmt.Sprintf("\U0001F4DA looking up: %s...", args.Query))
	}

	books, err := search.SearchBooks(args.Query, 3)
	if err != nil {
		log.Error("book_search failed", "err", err)
		return fmt.Sprintf("book search failed: %v", err)
	}

	formatted := search.FormatBookResults(books)

	// Accumulate in search context.
	if tctx.searchContext != "" {
		tctx.searchContext += "\n\n"
	}
	tctx.searchContext += fmt.Sprintf("## Book Search: %s\n\n%s", args.Query, formatted)

	// Save to DB for observability.
	tctx.store.SaveSearch(tctx.triggerMsgID, "book", args.Query, formatted, len(books))

	log.Infof("  book_search: %d results for %q", len(books), args.Query)
	return formatted
}

// --- Memory tool execution (unchanged from before) ---

// selfFactBlocklist contains phrases that indicate the agent is just
// restating its system prompt capabilities rather than saving a genuine
// learned observation. These get rejected before hitting the database.
var selfFactBlocklist = []string{
	"i can recall",
	"i am able to",
	"i have the ability",
	"my role is",
	"i am an ai",
	"i am mira",
	"my name is mira",
	"i should be",
	"i try to be",
	"i am designed to",
	"i was created to",
	"my purpose is",
	"i am here to",
	"i can remember",
	"i can help",
}

// styleBlocklist catches AI writing tics that poison the voice over time.
// Facts with these patterns get rejected so they don't leak into the
// system prompt and infect the conversational model's tone.
var styleBlocklist = []string{
	// Em dashes — the #1 offender
	"\u2014", // —
	"\u2013", // –

	// "Not just X, it's Y" and variants
	"not just",
	"it's not just",
	"not merely",

	// Grandiose/hollow language
	"significant moment",
	"significant trust",
	"deeply personal",
	"genuinely incredible",
	"a testament to",
	"speaks volumes",

	// Corporate AI speak
	"actively investing",
	"building a bridge",
	"creating a richer",
	"meta-level",
	"hold space",
	"holding space",

	// Hollow filler
	"it's worth noting",
	"it's important to",
	"fundamentally",
	"remarkably",
	"transformative",
	"delve",
	"foster",
	"leverage",
	"tapestry",
	"realm",
	"landscape",
	"embark",
	"harness",
	"utilize",
}

// maxFactLength is the hard limit on fact text length. Facts are supposed
// to be 1-2 sentences. Multi-paragraph reflections belong in the
// persona evolution system, not in individual facts.
const maxFactLength = 200

func execSaveFact(argsJSON string, subject string, tctx *toolContext) string {
	var args struct {
		Fact       string `json:"fact"`
		Category   string `json:"category"`
		Importance int    `json:"importance"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	if args.Importance < 1 {
		args.Importance = 1
	}
	if args.Importance > 10 {
		args.Importance = 10
	}

	// Quality gate for self-facts: block system-prompt restatements.
	if subject == "self" {
		lower := strings.ToLower(args.Fact)
		for _, blocked := range selfFactBlocklist {
			if strings.Contains(lower, blocked) {
				log.Warn("blocked self-fact (matches blocklist)", "blocklist_entry", blocked, "fact", args.Fact)
				return fmt.Sprintf("rejected: this is a system capability, not a learned observation. Self-facts should only capture things learned through interaction.")
			}
		}
	}

	// Style gate for ALL facts: reject AI writing tics.
	// Facts get injected into the system prompt, so sloppy style here
	// poisons the conversational model's tone over time. This is the
	// immune system against the AI-slop feedback loop.
	lower := strings.ToLower(args.Fact)
	for _, blocked := range styleBlocklist {
		if strings.Contains(lower, blocked) {
			log.Warn("blocked fact (style)", "pattern", blocked, "fact", args.Fact)
			return fmt.Sprintf("rejected: rewrite this fact in plain, concise language. Avoid em dashes, 'not just X it's Y', and grandiose phrasing. Keep it under 2 sentences. The blocked pattern was: %q", blocked)
		}
	}

	// Length gate: facts should be 1-2 sentences, not paragraphs.
	if len(args.Fact) > maxFactLength {
		log.Warn("blocked fact (too long)", "len", len(args.Fact), "fact", args.Fact[:100])
		return fmt.Sprintf("rejected: fact is %d characters (max %d). Condense to 1-2 short sentences.", len(args.Fact), maxFactLength)
	}

	// Auto-inject timestamp for "context" category facts.
	if args.Category == "context" {
		tz := tctx.cfg.Scheduler.Timezone
		loc, err := time.LoadLocation(tz)
		if err != nil {
			loc = time.UTC
		}
		stamp := time.Now().In(loc).Format("2006-01-02")
		args.Fact = fmt.Sprintf("[%s] %s", stamp, args.Fact)
	}

	// Semantic duplicate check using embeddings.
	var newVec []float32
	if tctx.embedClient != nil {
		var err error
		newVec, err = tctx.embedClient.Embed(args.Fact)
		if err != nil {
			log.Warn("embedding failed, skipping duplicate check", "err", err)
		} else {
			if duplicate, existingID, existingFact, sim := checkDuplicate(newVec, subject, tctx); duplicate {
				log.Info("blocked duplicate fact", "similarity_pct", sim*100, "existing_id", existingID, "fact", args.Fact)
				return fmt.Sprintf("rejected: too similar (%.0f%%) to existing fact ID=%d (%q). Use update_fact to refine it instead.",
					sim*100, existingID, existingFact)
			}
		}
	}

	id, err := tctx.store.SaveFact(args.Fact, args.Category, subject, 0, args.Importance, newVec)
	if err != nil {
		return fmt.Sprintf("error saving fact: %v", err)
	}
	label := "user fact"
	if subject == "self" {
		label = "self fact"
	}

	tctx.savedFacts = append(tctx.savedFacts, args.Fact)

	return fmt.Sprintf("saved %s ID=%d: %s", label, id, args.Fact)
}

func checkDuplicate(newVec []float32, subject string, tctx *toolContext) (isDuplicate bool, existingID int64, existingFact string, similarity float64) {
	existingFacts, err := tctx.store.AllActiveFacts()
	if err != nil {
		log.Warn("couldn't load facts for duplicate check", "err", err)
		return false, 0, "", 0
	}

	var bestSim float64
	var bestID int64
	var bestFact string

	for _, existing := range existingFacts {
		if existing.Subject != subject {
			continue
		}

		existVec := existing.Embedding
		if len(existVec) == 0 {
			existVec, err = tctx.embedClient.Embed(existing.Fact)
			if err != nil {
				continue
			}
			_ = tctx.store.UpdateFactEmbedding(existing.ID, existVec)
			log.Debug("backfilled embedding for fact", "fact_id", existing.ID)
		}

		sim := embed.CosineSimilarity(newVec, existVec)
		if sim > bestSim {
			bestSim = sim
			bestID = existing.ID
			bestFact = existing.Fact
		}
	}

	if bestSim >= tctx.similarityThreshold {
		return true, bestID, bestFact, bestSim
	}
	return false, 0, "", 0
}

func execUpdateFact(argsJSON string, tctx *toolContext) string {
	var args struct {
		FactID     int64  `json:"fact_id"`
		Fact       string `json:"fact"`
		Category   string `json:"category"`
		Importance int    `json:"importance"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	if args.Importance < 1 {
		args.Importance = 1
	}
	if args.Importance > 10 {
		args.Importance = 10
	}

	// Same style and length gates as save_fact — updates shouldn't
	// sneak in AI-slop or paragraphs either.
	lower := strings.ToLower(args.Fact)
	for _, blocked := range styleBlocklist {
		if strings.Contains(lower, blocked) {
			log.Warn("blocked fact update (style)", "pattern", blocked, "fact", args.Fact)
			return fmt.Sprintf("rejected: rewrite in plain, concise language. Blocked pattern: %q", blocked)
		}
	}
	if len(args.Fact) > maxFactLength {
		log.Warn("blocked fact update (too long)", "len", len(args.Fact))
		return fmt.Sprintf("rejected: fact is %d characters (max %d). Condense to 1-2 short sentences.", len(args.Fact), maxFactLength)
	}

	// Auto-inject timestamp for "context" category facts. These are
	// ephemeral day-to-day facts (like "working on X today") that need
	// a date stamp so they age out naturally. The agent doesn't need to
	// worry about timestamps — we inject them behind the scenes.
	if args.Category == "context" {
		tz := tctx.cfg.Scheduler.Timezone
		loc, err := time.LoadLocation(tz)
		if err != nil {
			loc = time.UTC
		}
		stamp := time.Now().In(loc).Format("2006-01-02")
		args.Fact = fmt.Sprintf("[%s] %s", stamp, args.Fact)
	}

	if err := tctx.store.UpdateFact(args.FactID, args.Fact, args.Category, args.Importance); err != nil {
		return fmt.Sprintf("error updating fact: %v", err)
	}

	if tctx.embedClient != nil {
		if newVec, err := tctx.embedClient.Embed(args.Fact); err == nil {
			_ = tctx.store.UpdateFactEmbedding(args.FactID, newVec)
			log.Debug("recomputed embedding for updated fact", "fact_id", args.FactID)
		}
	}

	return fmt.Sprintf("updated fact ID=%d: %s", args.FactID, args.Fact)
}

func execRemoveFact(argsJSON string, store *memory.Store) string {
	var args struct {
		FactID int64  `json:"fact_id"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	if err := store.DeactivateFact(args.FactID); err != nil {
		return fmt.Sprintf("error removing fact: %v", err)
	}
	return fmt.Sprintf("removed fact ID=%d (reason: %s)", args.FactID, args.Reason)
}

func execUpdatePersona(argsJSON string, store *memory.Store, personaFile string) string {
	var args struct {
		Content string `json:"content"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	if err := os.WriteFile(personaFile, []byte(args.Content), 0644); err != nil {
		return fmt.Sprintf("error writing persona file: %v", err)
	}

	id, err := store.SavePersonaVersion(args.Content, "agent: "+args.Reason)
	if err != nil {
		return fmt.Sprintf("persona file updated but failed to save version: %v", err)
	}

	return fmt.Sprintf("persona updated (version ID=%d, reason: %s)", id, args.Reason)
}

// truncateLog shortens a string for log output, adding "..." if it was cut.
// mustJSON marshals a string to a JSON string literal (with quotes and
// escaping). Used to safely embed agent text into a JSON object without
// risking broken JSON from quotes or newlines in the content.
func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func truncateLog(s string, maxLen int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// formatTraceLine builds an HTML-formatted trace line for a single tool call.
// Each tool type gets its own emoji and formatting style so you can scan
// the trace message at a glance.
func formatTraceLine(toolName, argsJSON, result string) string {
	switch toolName {
	case "think":
		// Show the full thinking text in italics.
		var args struct {
			Thought string `json:"thought"`
		}
		json.Unmarshal([]byte(argsJSON), &args)
		return fmt.Sprintf("🧠 <b>think:</b> <i>%s</i>", escapeHTML(args.Thought))

	case "reply":
		// Show the instruction (what the agent told Mira to say) in italics.
		// Don't show the actual reply text — that's already visible as a message.
		var args struct {
			Instruction string `json:"instruction"`
		}
		json.Unmarshal([]byte(argsJSON), &args)
		return fmt.Sprintf("📝 <b>reply:</b> <i>%s</i>", escapeHTML(truncateLog(args.Instruction, 200)))

	case "save_fact":
		// Show full fact details — category, importance, and the fact text.
		var args struct {
			Fact       string `json:"fact"`
			Category   string `json:"category"`
			Importance int    `json:"importance"`
		}
		json.Unmarshal([]byte(argsJSON), &args)
		return fmt.Sprintf("💾 <b>save_fact:</b> %s\n    category=%s, importance=%d", escapeHTML(args.Fact), args.Category, args.Importance)

	case "update_fact":
		var args struct {
			FactID     int    `json:"fact_id"`
			Fact       string `json:"fact"`
			Category   string `json:"category"`
			Importance int    `json:"importance"`
		}
		json.Unmarshal([]byte(argsJSON), &args)
		return fmt.Sprintf("📝 <b>update_fact:</b> #%d → %s\n    category=%s, importance=%d", args.FactID, escapeHTML(args.Fact), args.Category, args.Importance)

	case "remove_fact":
		var args struct {
			FactID int64  `json:"fact_id"`
			Reason string `json:"reason"`
		}
		json.Unmarshal([]byte(argsJSON), &args)
		return fmt.Sprintf("🗑 <b>remove_fact:</b> #%d — %s", args.FactID, escapeHTML(args.Reason))

	case "save_self_fact":
		var args struct {
			Fact       string `json:"fact"`
			Category   string `json:"category"`
			Importance int    `json:"importance"`
		}
		json.Unmarshal([]byte(argsJSON), &args)
		return fmt.Sprintf("🪞 <b>save_self_fact:</b> %s\n    category=%s, importance=%d", escapeHTML(args.Fact), args.Category, args.Importance)

	case "web_search":
		var args struct {
			Query string `json:"query"`
		}
		json.Unmarshal([]byte(argsJSON), &args)
		return fmt.Sprintf("🔍 <b>web_search:</b> \"%s\"\n    → %s", escapeHTML(args.Query), escapeHTML(truncateLog(result, 80)))

	case "web_read":
		var args struct {
			URL string `json:"url"`
		}
		json.Unmarshal([]byte(argsJSON), &args)
		return fmt.Sprintf("🌐 <b>web_read:</b> %s\n    → %s", escapeHTML(truncateLog(args.URL, 60)), escapeHTML(truncateLog(result, 80)))

	case "book_search":
		var args struct {
			Query string `json:"query"`
		}
		json.Unmarshal([]byte(argsJSON), &args)
		return fmt.Sprintf("📚 <b>book_search:</b> \"%s\"\n    → %s", escapeHTML(args.Query), escapeHTML(truncateLog(result, 80)))

	case "view_image":
		return fmt.Sprintf("👁 <b>view_image:</b> → %s", escapeHTML(truncateLog(result, 80)))

	case "recall_memories":
		var args struct {
			Query string `json:"query"`
		}
		json.Unmarshal([]byte(argsJSON), &args)
		return fmt.Sprintf("🔎 <b>recall_memories:</b> \"%s\"\n    → %s", escapeHTML(args.Query), escapeHTML(truncateLog(result, 80)))

	case "log_mood":
		var args struct {
			Rating int    `json:"rating"`
			Note   string `json:"note"`
		}
		json.Unmarshal([]byte(argsJSON), &args)
		return fmt.Sprintf("😊 <b>log_mood:</b> %d/5 — %s", args.Rating, escapeHTML(args.Note))

	case "create_reminder":
		var args struct {
			Message     string `json:"message"`
			NaturalTime string `json:"natural_time"`
		}
		json.Unmarshal([]byte(argsJSON), &args)
		return fmt.Sprintf("⏰ <b>create_reminder:</b> \"%s\" at %s", escapeHTML(args.Message), escapeHTML(args.NaturalTime))

	case "create_schedule":
		var args struct {
			Name     string `json:"name"`
			CronExpr string `json:"cron_expr"`
		}
		json.Unmarshal([]byte(argsJSON), &args)
		return fmt.Sprintf("📅 <b>create_schedule:</b> \"%s\" (%s)", escapeHTML(args.Name), args.CronExpr)

	case "set_location":
		var args struct {
			Query string `json:"query"`
		}
		json.Unmarshal([]byte(argsJSON), &args)
		return fmt.Sprintf("📍 <b>set_location:</b> %s", escapeHTML(args.Query))

	case "update_persona":
		var args struct {
			Reason string `json:"reason"`
		}
		json.Unmarshal([]byte(argsJSON), &args)
		return fmt.Sprintf("✨ <b>update_persona:</b> %s", escapeHTML(args.Reason))

	case "no_action":
		return "➖ <b>no_action</b>"

	case "done":
		return "✅ <b>done</b>"

	case "get_current_time":
		return fmt.Sprintf("🕐 <b>get_current_time:</b> → %s", escapeHTML(truncateLog(result, 60)))

	default:
		return fmt.Sprintf("🔧 <b>%s:</b> → %s", escapeHTML(toolName), escapeHTML(truncateLog(result, 80)))
	}
}

// escapeHTML escapes special characters for Telegram's HTML parse mode.
func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

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
	Cfg                 *config.Config
	ScrubbedUserMessage string
	ScrubVault          *scrub.Vault
	ConversationID      string
	TriggerMsgID        int64
	StatusCallback      StatusCallback
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
	recentMsgs, err := params.Store.RecentMessages(params.ConversationID, params.Cfg.Memory.RecentMessages)
	if err != nil {
		log.Error("loading history", "err", err)
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

	// Build the context message for the agent.
	context := buildAgentContext(params.ScrubbedUserMessage, recentMsgs, userFacts, selfFacts, params.ImageBase64 != "")

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
		chatLLM:             params.ChatLLM,
		visionLLM:           params.VisionLLM,
		tavilyClient:        params.TavilyClient,
		cfg:                 params.Cfg,
		scrubVault:          params.ScrubVault,
		scrubbedUserMessage: params.ScrubbedUserMessage,
		conversationID:      params.ConversationID,
		triggerMsgID:        params.TriggerMsgID,
		conversationSummary: conversationSummary,
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
	for i := 0; i < 10; i++ {
		resp, err := params.AgentLLM.ChatCompletionWithTools(messages, tools)
		if err != nil {
			log.Error("LLM error", "err", err)
			break
		}

		// Log agent metrics linked to the user message that triggered this run.
		params.Store.SaveMetric(resp.Model, resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens, resp.CostUSD, 0, params.TriggerMsgID)
		log.Infof("  tokens: %d prompt + %d completion | $%.6f",
			resp.PromptTokens, resp.CompletionTokens, resp.CostUSD)

		// If no tool calls, the agent is done.
		if len(resp.ToolCalls) == 0 {
			if resp.Content != "" {
				log.Infof("  done (text): %s", resp.Content)
			} else {
				log.Info("  done (no actions)")
			}
			break
		}

		log.Infof("  %d tool call(s):", len(resp.ToolCalls))

		// Append the assistant message with tool calls to the conversation.
		messages = append(messages, llm.ChatMessage{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})

		// Execute each tool call and collect results.
		// Save every step to agent_turns for full observability.
		for _, tc := range resp.ToolCalls {
			// Save the tool call (what the agent decided to do).
			params.Store.SaveAgentTurn(params.TriggerMsgID, turnIndex, "assistant", tc.Function.Name, tc.Function.Arguments, "")
			turnIndex++

			result := executeTool(tc, tctx)
			log.Infof("    → %s: %s", tc.Function.Name, truncateLog(result, 200))

			// Save the tool result (what happened when we executed it).
			params.Store.SaveAgentTurn(params.TriggerMsgID, turnIndex, "tool", tc.Function.Name, "", result)
			turnIndex++

			messages = append(messages, llm.ChatMessage{
				Role:       "tool",
				Content:    result,
				ToolCallID: tc.ID,
			})
		}

		// Exit when the agent explicitly signals it's done.
		if tctx.doneCalled {
			log.Info("  done signal received")
			break
		}
	}

	// Safety net: if the agent never called reply, generate a fallback
	// response directly. This should be rare with a well-tuned prompt,
	// but we never want the user to see just a placeholder.
	if !tctx.replyCalled {
		log.Warn("reply was never called, generating fallback")
		fallbackResult := execReply(`{"instruction":"The user sent a message. Respond naturally and conversationally."}`, tctx)
		if !tctx.replyCalled {
			log.Error("fallback reply also failed", "result", fallbackResult)
			return nil, fmt.Errorf("agent failed to generate a reply")
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

		// Trigger: Persona rewrite — have enough reflections accumulated since the last rewrite?
		if params.RewriteEveryN > 0 {
			reflectionCount, err := params.Store.ReflectionCountSinceLastRewrite()
			if err != nil {
				log.Error("checking reflection count for rewrite trigger", "err", err)
			} else if reflectionCount >= params.RewriteEveryN {
				log.Infof("  [persona] rewrite triggered (%d reflections, threshold: %d)", reflectionCount, params.RewriteEveryN)
				if rewritten, err := persona.MaybeRewrite(params.ChatLLM, params.Store, params.Cfg.Persona.PersonaFile, 0); err != nil {
					log.Error("persona rewrite error", "err", err)
				} else if rewritten {
					log.Info("persona.md rewritten")
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
func buildAgentContext(userMessage string, history []memory.Message, userFacts, selfFacts []memory.Fact, hasImage bool) string {
	var b strings.Builder

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
func executeTool(tc llm.ToolCall, tctx *toolContext) string {
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
	case "think":
		return execThink(tc.Function.Arguments, tctx)
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

	// Build the user message. If we have search context, we inject it
	// as a system note before the user's actual message so the model
	// knows what information is available.
	userContent := tctx.scrubbedUserMessage
	if fullContext != "" {
		userContent = fmt.Sprintf("[Search/reference context for this response — use this information naturally, don't quote it verbatim or mention that you searched unless appropriate:]\n\n%s\n\n[End context]\n\n[Agent instruction: %s]\n\n%s",
			fullContext, args.Instruction, tctx.scrubbedUserMessage)
	} else {
		userContent = fmt.Sprintf("[Agent instruction: %s]\n\n%s",
			args.Instruction, tctx.scrubbedUserMessage)
	}
	llmMessages = append(llmMessages, llm.ChatMessage{
		Role:    "user",
		Content: userContent,
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

	// Send the response to Telegram by editing the placeholder message.
	if tctx.statusCallback != nil {
		if err := tctx.statusCallback(replyText); err != nil {
			log.Error("reply: sending to Telegram", "err", err)
		}
	}

	tctx.replyCalled = true
	tctx.replyText = replyText

	return fmt.Sprintf("reply sent (%d chars)", len(replyText))
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

	// Layer 3: Memory context — extracted facts about the user and self.
	if memCtx, err := memory.BuildMemoryContext(tctx.store, tctx.cfg.Memory.MaxFactsInContext); err == nil && memCtx != "" {
		parts = append(parts, memCtx)
	}

	// Layer 4: Conversation summary — compacted older messages.
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

	// Quality gate for self-facts.
	if subject == "self" {
		lower := strings.ToLower(args.Fact)
		for _, blocked := range selfFactBlocklist {
			if strings.Contains(lower, blocked) {
				log.Warn("blocked self-fact (matches blocklist)", "blocklist_entry", blocked, "fact", args.Fact)
				return fmt.Sprintf("rejected: this is a system capability, not a learned observation. Self-facts should only capture things learned through interaction.")
			}
		}
	}

	// Semantic duplicate check using embeddings.
	var newVec []float64
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

func checkDuplicate(newVec []float64, subject string, tctx *toolContext) (isDuplicate bool, existingID int64, existingFact string, similarity float64) {
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
func truncateLog(s string, maxLen int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

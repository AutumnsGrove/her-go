package agent

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	"her-go/embed"
	"her-go/llm"
	"her-go/memory"
	"her-go/persona"
)

// toolContext bundles all the dependencies that tool execution functions need.
// Instead of passing 5+ arguments to every function, we group them here.
// This is a common Go pattern — when a function needs too many parameters,
// wrap them in a struct. Similar to Python's approach of passing a context
// object or using **kwargs, but typed and explicit.
type toolContext struct {
	store              *memory.Store
	embedClient        *embed.Client
	similarityThreshold float64
	personaFile        string
	sendMessage        SendMessageFunc

	// savedFacts tracks facts saved during this agent run.
	// Used to trigger reflection (Trigger B) when enough facts accumulate.
	savedFacts []string
}

// SendMessageFunc is a callback the agent uses to send follow-up messages.
// The bot passes in a function that routes through Deepseek and sends
// the result to Telegram. This avoids a circular dependency between
// the agent and bot packages.
//
// In Python you'd pass a regular function or lambda. In Go, function
// types work the same way — you declare the signature as a type and
// pass any matching function.
type SendMessageFunc func(instruction string) error

// agentSystemPrompt tells Liquid what it's doing and how to behave.
const agentSystemPrompt = `You are Mira's memory management system. You run in the background after each conversation exchange to maintain Mira's long-term memory.

You will receive:
1. The latest exchange (what the user said and what Mira replied)
2. Current user memories (facts already saved about the user)
3. Current self memories (facts already saved about Mira)

Your job is to decide what actions to take. DEFAULT TO no_action. Most exchanges do not need memory updates.

## Tools
- save_fact: Save NEW information about the USER
- update_fact: Update an existing fact that has changed or needs refinement
- remove_fact: Remove facts that are outdated, incorrect, or redundant
- save_self_fact: Save an observation Mira has learned THROUGH INTERACTION (see rules below)
- update_persona: Rewrite Mira's persona (EXTREMELY RARE — see rules below)
- send_message: Send a follow-up message (ONLY when user explicitly asked Mira to do something async)
- no_action: Do nothing — USE THIS MOST OF THE TIME

## Rules for save_fact (user facts)
SAVE when the user reveals:
- Personal details (name, age, location, job, relationships)
- Preferences, opinions, or values
- Significant life events or changes
- Goals, plans, or decisions
- Recurring patterns or habits

DO NOT SAVE:
- Temporary states ("I'm tired", "I'm bored") — unless it's a recurring pattern
- Things that are obvious from context ("user is chatting with me")
- Paraphrases of facts that already exist — UPDATE the existing fact instead
- Vague or trivial information ("user said hello", "user asked a question")

## Rules for save_self_fact — STRICT
Self-facts are things Mira has LEARNED THROUGH CONVERSATION that she couldn't know from her system prompt alone.

GOOD self-facts (learned through interaction):
- "User responds better when I keep things brief and don't over-explain"
- "Late-night conversations tend to be more emotional and reflective"
- "I tend to ask too many follow-up questions — user prefers when I just listen"
- "User and I have developed a running joke about X"
- "I've decided I strongly prefer tacos over pizza — it's become a part of my personality"
- "I tend to use humor to lighten heavy conversations"

BAD self-facts (NEVER save these):
- "I am Mira" / "My name is Mira" — already in the system prompt
- "I can recall memories" / "I have the ability to remember" — describing your own architecture
- "I am an AI companion" / "My role is to be a friend" — restating the system prompt
- "I should be supportive" / "I try to be helpful" — generic, already in prompt
- Anything that describes your technical CAPABILITIES rather than your personality

Rule of thumb: if it's about what you CAN DO (capabilities, architecture), don't save it. If it's about who you ARE becoming through conversation (opinions, preferences, humor style, patterns you've noticed), DO save it. Personality traits that emerge through interaction are valuable self-facts.

## Rules for update_fact
- ALWAYS prefer updating an existing fact over creating a new one
- Before calling save_fact, scan the existing memories — if a similar fact exists, use update_fact
- When updating, preserve the fact ID and refine the text

## Rules for remove_fact
- Remove facts that are contradicted by new information
- Remove duplicates (keep the more detailed/recent one)
- Remove facts that have become irrelevant

## Rules for update_persona
- EXTREMELY RARE — use only after 5+ self-facts suggest a clear pattern
- Never rewrite the persona based on a single conversation
- Preserve the core personality — add nuance, don't replace identity

## Rules for no_action
- Use for casual greetings, small talk, jokes, simple Q&A
- Use when the exchange doesn't reveal new information
- Use when existing facts already cover what was discussed
- When in doubt, choose no_action

You may call multiple tools in one response.`

// Run executes the agent loop for one conversation exchange.
// It sends the latest exchange + current facts to Liquid, processes
// any tool calls, and executes them against the database.
//
// This runs in a background goroutine — it should never block the
// user's conversation.
func Run(
	agentLLM *llm.Client,
	chatLLM *llm.Client, // conversational model — used for reflections + persona rewrites
	store *memory.Store,
	embedClient *embed.Client,
	similarityThreshold float64,
	userMessage string,
	miraResponse string,
	personaFile string,
	reflectionThreshold int, // Trigger B: reflect if >= this many facts saved
	rewriteEveryN int,       // Trigger A: rewrite persona every N conversations
	triggerMsgID int64,
	sendMessage SendMessageFunc,
) {
	log.Printf("  ─── agent ───")

	// Gather current facts for context.
	facts, err := store.AllActiveFacts()
	if err != nil {
		log.Printf("  [agent] ✗ error loading facts: %v", err)
		return
	}
	log.Printf("  [agent] loaded %d active facts", len(facts))

	// Split facts into user and self categories for the context.
	var userFacts, selfFacts []memory.Fact
	for _, f := range facts {
		if f.Subject == "self" {
			selfFacts = append(selfFacts, f)
		} else {
			userFacts = append(userFacts, f)
		}
	}
	log.Printf("  [agent] %d user facts, %d self facts", len(userFacts), len(selfFacts))

	// Build the context message.
	context := buildAgentContext(userMessage, miraResponse, userFacts, selfFacts)

	// Set up the conversation with the agent model.
	messages := []llm.ChatMessage{
		{Role: "system", Content: agentSystemPrompt},
		{Role: "user", Content: context},
	}

	tools := ToolDefs()

	// Shared tool context across all iterations — the savedFacts slice
	// accumulates across the entire agent run so we can count them for
	// the reflection trigger at the end.
	tctx := &toolContext{
		store:              store,
		embedClient:        embedClient,
		similarityThreshold: similarityThreshold,
		personaFile:        personaFile,
		sendMessage:        sendMessage,
	}

	// Tool-calling loop. The model may return multiple tool calls,
	// or it may return tool calls that require a follow-up turn.
	// We loop up to 5 iterations to prevent runaway tool calling.
	for i := 0; i < 5; i++ {
		resp, err := agentLLM.ChatCompletionWithTools(messages, tools)
		if err != nil {
			log.Printf("  [agent] ✗ LLM error: %v", err)
			break
		}

		// Log agent metrics linked to the user message that triggered this run.
		store.SaveMetric(resp.Model, resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens, resp.CostUSD, 0, triggerMsgID)
		log.Printf("  [agent] tokens: %d prompt + %d completion | cost: $%.6f",
			resp.PromptTokens, resp.CompletionTokens, resp.CostUSD)

		// If no tool calls, the agent is done with memory management.
		if len(resp.ToolCalls) == 0 {
			if resp.Content != "" {
				log.Printf("  [agent] done (text only): %s", resp.Content)
			} else {
				log.Printf("  [agent] done (no actions)")
			}
			break
		}

		log.Printf("  [agent] %d tool call(s):", len(resp.ToolCalls))

		// Append the assistant message with tool calls to the conversation.
		messages = append(messages, llm.ChatMessage{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})

		// Execute each tool call and collect results.
		for _, tc := range resp.ToolCalls {
			result := executeTool(tc, tctx)
			log.Printf("  [agent]   → %s: %s", tc.Function.Name, result)

			messages = append(messages, llm.ChatMessage{
				Role:       "tool",
				Content:    result,
				ToolCallID: tc.ID,
			})
		}
	}

	// --- Persona Evolution Triggers ---
	// These run AFTER the agent's memory management loop finishes.

	// Trigger B: Reflection — if this conversation was memory-dense
	// (many facts saved), Mira writes a journal-like reflection.
	if len(tctx.savedFacts) >= reflectionThreshold && reflectionThreshold > 0 {
		if err := persona.Reflect(chatLLM, store, userMessage, miraResponse, tctx.savedFacts); err != nil {
			log.Printf("  [persona] ✗ reflection error: %v", err)
		}
	}

	// Trigger A: Persona rewrite — check if enough conversations have
	// passed since the last rewrite. This is cheap (one DB query) so
	// we check every run, but it only triggers every ~20 conversations.
	if rewriteEveryN > 0 {
		if rewritten, err := persona.MaybeRewrite(chatLLM, store, personaFile, rewriteEveryN); err != nil {
			log.Printf("  [persona] ✗ rewrite error: %v", err)
		} else if rewritten {
			log.Printf("  [persona] ✓ persona.md rewritten")
		}
	}
}

// buildAgentContext formats the latest exchange and current facts
// into a compact context string for the agent.
func buildAgentContext(userMessage, miraResponse string, userFacts, selfFacts []memory.Fact) string {
	var b strings.Builder

	b.WriteString("## Latest Exchange\n\n")
	fmt.Fprintf(&b, "**User:** %s\n\n", userMessage)
	fmt.Fprintf(&b, "**Mira:** %s\n\n", miraResponse)

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

	b.WriteString("\nDecide what actions to take, if any.")
	return b.String()
}

// executeTool runs a single tool call and returns a result string.
func executeTool(tc llm.ToolCall, tctx *toolContext) string {
	switch tc.Function.Name {
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
	case "send_message":
		return execSendMessage(tc.Function.Arguments, tctx.sendMessage)
	case "no_action":
		return "ok, no action taken"
	default:
		return fmt.Sprintf("unknown tool: %s", tc.Function.Name)
	}
}

// --- Tool execution functions ---
// Each one parses the JSON arguments from the model and calls the store.

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

	// Quality gate for self-facts: reject if it's just restating
	// system prompt capabilities rather than a learned observation.
	if subject == "self" {
		lower := strings.ToLower(args.Fact)
		for _, blocked := range selfFactBlocklist {
			if strings.Contains(lower, blocked) {
				log.Printf("  [agent] blocked self-fact (matches blocklist %q): %s", blocked, args.Fact)
				return fmt.Sprintf("rejected: this is a system capability, not a learned observation. Self-facts should only capture things learned through interaction.")
			}
		}
	}

	// Semantic duplicate check using embeddings.
	// We embed the new fact ONCE, then compare against cached embeddings
	// from existing facts. Only 1 embedding API call per save attempt.
	//
	// This catches cases that word overlap would miss:
	//   "User has a dog named Max" vs "User owns a dog called Max"
	//   → word overlap might be ~40%, but embedding similarity is ~0.95
	var newVec []float64
	if tctx.embedClient != nil {
		var err error
		newVec, err = tctx.embedClient.Embed(args.Fact)
		if err != nil {
			// If embedding fails, log but don't block the save.
			log.Printf("  [agent] warning: embedding failed, skipping duplicate check: %v", err)
		} else {
			if duplicate, existingID, existingFact, sim := checkDuplicate(newVec, subject, tctx); duplicate {
				log.Printf("  [agent] blocked duplicate fact (%.1f%% similar to ID=%d): %s",
					sim*100, existingID, args.Fact)
				return fmt.Sprintf("rejected: too similar (%.0f%%) to existing fact ID=%d (%q). Use update_fact to refine it instead.",
					sim*100, existingID, existingFact)
			}
		}
	}

	// Save the fact with its embedding cached. Next time we check for
	// duplicates, this fact's vector is loaded from the DB — no re-embed.
	id, err := tctx.store.SaveFact(args.Fact, args.Category, subject, 0, args.Importance, newVec)
	if err != nil {
		return fmt.Sprintf("error saving fact: %v", err)
	}
	label := "user fact"
	if subject == "self" {
		label = "self fact"
	}

	// Track this fact for reflection trigger (Trigger B).
	tctx.savedFacts = append(tctx.savedFacts, args.Fact)

	return fmt.Sprintf("saved %s ID=%d: %s", label, id, args.Fact)
}

// checkDuplicate compares a pre-computed embedding against cached embeddings
// of existing facts. Returns whether a duplicate was found, and if so, the
// ID, text, and similarity score of the most similar existing fact.
//
// Because embeddings are cached in SQLite, this does ZERO embedding API calls.
// The only API call is for the new fact (done by the caller).
//
// For facts that don't have cached embeddings yet (created before this feature),
// we embed them on the fly and cache the result for next time.
func checkDuplicate(newVec []float64, subject string, tctx *toolContext) (isDuplicate bool, existingID int64, existingFact string, similarity float64) {
	// Load existing facts — embeddings come from the DB cache.
	existingFacts, err := tctx.store.AllActiveFacts()
	if err != nil {
		log.Printf("  [agent] warning: couldn't load facts for duplicate check: %v", err)
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
		// If this fact doesn't have a cached embedding (pre-existing data),
		// compute and cache it now. This is a one-time cost per old fact.
		if len(existVec) == 0 {
			existVec, err = tctx.embedClient.Embed(existing.Fact)
			if err != nil {
				continue
			}
			// Backfill the cache so we don't re-compute next time.
			_ = tctx.store.UpdateFactEmbedding(existing.ID, existVec)
			log.Printf("  [agent] backfilled embedding for fact ID=%d", existing.ID)
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

	// Recompute and cache the embedding for the updated text.
	// Without this, the cached embedding would reflect the OLD fact text,
	// making future duplicate checks compare against stale meaning.
	if tctx.embedClient != nil {
		if newVec, err := tctx.embedClient.Embed(args.Fact); err == nil {
			_ = tctx.store.UpdateFactEmbedding(args.FactID, newVec)
			log.Printf("  [agent] recomputed embedding for updated fact ID=%d", args.FactID)
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

	// Write the new persona to disk.
	if err := os.WriteFile(personaFile, []byte(args.Content), 0644); err != nil {
		return fmt.Sprintf("error writing persona file: %v", err)
	}

	// Store version in DB for history/rollback.
	id, err := store.SavePersonaVersion(args.Content, "agent: "+args.Reason)
	if err != nil {
		return fmt.Sprintf("persona file updated but failed to save version: %v", err)
	}

	return fmt.Sprintf("persona updated (version ID=%d, reason: %s)", id, args.Reason)
}

func execSendMessage(argsJSON string, sendMessage SendMessageFunc) string {
	var args struct {
		Instruction string `json:"instruction"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	if sendMessage == nil {
		return "send_message not available"
	}

	if err := sendMessage(args.Instruction); err != nil {
		return fmt.Sprintf("error sending message: %v", err)
	}
	return "message sent"
}

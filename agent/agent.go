package agent

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	"her-go/llm"
	"her-go/memory"
)

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
const agentSystemPrompt = `You are Mira's memory management system. You run in the background after each conversation exchange to maintain Mira's long-term memory and self-knowledge.

You will receive:
1. The latest exchange (what the user said and what Mira replied)
2. User memories (facts about the person Mira is talking to)
3. Self memories (facts about Mira herself — her identity, patterns, observations)

Your job is to decide what actions to take using the available tools:
- save_fact: Save NEW information about the USER worth remembering
- update_fact: Update an existing fact (user or self) that has changed
- remove_fact: Remove facts that are outdated, incorrect, or redundant
- save_self_fact: Save something about MIRA — her own observations, patterns, what works in conversation, her identity
- update_persona: Rewrite Mira's persona description when her self-understanding has meaningfully evolved
- send_message: Send a follow-up message (ONLY when the user asked Mira to DO something)
- no_action: Do nothing (most casual exchanges need no memory updates)

Guidelines:
- Be selective. Not every message needs a memory update. Casual greetings and small talk usually warrant no_action.
- Avoid duplicate facts. Check existing memories before saving — if a similar fact exists, update it instead.
- Consolidate when possible. If two facts say basically the same thing, remove the weaker one and update the other.
- Mood facts are ephemeral. Don't save temporary moods like "user is tired" unless it's a recurring pattern.
- Identity and preference facts are high value.
- Update > Save. If something already exists as a fact, update it rather than creating a duplicate.

Self-fact guidelines:
- save_self_fact is for Mira's OWN knowledge about herself: "My name is Mira", "User responds well when I keep things brief", "I tend to ask too many questions in a row"
- These are observations about communication patterns, relationship dynamics, and Mira's own identity
- Do NOT use save_self_fact for facts about the user — use save_fact for those

Persona guidelines:
- update_persona should be RARE — only when multiple self-facts suggest a meaningful shift in how Mira sees herself
- The persona should evolve gradually, not swing wildly after a single conversation
- Preserve the core personality — evolution, not replacement

You may call multiple tools in one response.`

// Run executes the agent loop for one conversation exchange.
// It sends the latest exchange + current facts to Liquid, processes
// any tool calls, and executes them against the database.
//
// This runs in a background goroutine — it should never block the
// user's conversation.
func Run(
	agentLLM *llm.Client,
	store *memory.Store,
	userMessage string,
	miraResponse string,
	personaFile string,
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

	// Set up the conversation with Liquid.
	messages := []llm.ChatMessage{
		{Role: "system", Content: agentSystemPrompt},
		{Role: "user", Content: context},
	}

	tools := ToolDefs()

	// Tool-calling loop. The model may return multiple tool calls,
	// or it may return tool calls that require a follow-up turn.
	// We loop up to 5 iterations to prevent runaway tool calling.
	for i := 0; i < 5; i++ {
		resp, err := agentLLM.ChatCompletionWithTools(messages, tools)
		if err != nil {
			log.Printf("  [agent] ✗ LLM error: %v", err)
			return
		}

		// Log agent metrics to the DB (message_id 0 → NULL).
		store.SaveMetric(resp.Model, resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens, resp.CostUSD, 0, 0)
		log.Printf("  [agent] tokens: %d prompt + %d completion | cost: $%.6f",
			resp.PromptTokens, resp.CompletionTokens, resp.CostUSD)

		// If no tool calls, the agent is done.
		if len(resp.ToolCalls) == 0 {
			if resp.Content != "" {
				log.Printf("  [agent] done (text only): %s", resp.Content)
			} else {
				log.Printf("  [agent] done (no actions)")
			}
			return
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
			result := executeTool(tc, store, personaFile, sendMessage)
			log.Printf("  [agent]   → %s: %s", tc.Function.Name, result)

			messages = append(messages, llm.ChatMessage{
				Role:       "tool",
				Content:    result,
				ToolCallID: tc.ID,
			})
		}
	}

	log.Printf("  [agent] hit max iterations, stopping")
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
func executeTool(tc llm.ToolCall, store *memory.Store, personaFile string, sendMessage SendMessageFunc) string {
	switch tc.Function.Name {
	case "save_fact":
		return execSaveFact(tc.Function.Arguments, "user", store)
	case "save_self_fact":
		return execSaveFact(tc.Function.Arguments, "self", store)
	case "update_fact":
		return execUpdateFact(tc.Function.Arguments, store)
	case "remove_fact":
		return execRemoveFact(tc.Function.Arguments, store)
	case "update_persona":
		return execUpdatePersona(tc.Function.Arguments, store, personaFile)
	case "send_message":
		return execSendMessage(tc.Function.Arguments, sendMessage)
	case "no_action":
		return "ok, no action taken"
	default:
		return fmt.Sprintf("unknown tool: %s", tc.Function.Name)
	}
}

// --- Tool execution functions ---
// Each one parses the JSON arguments from the model and calls the store.

func execSaveFact(argsJSON string, subject string, store *memory.Store) string {
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

	id, err := store.SaveFact(args.Fact, args.Category, subject, 0, args.Importance)
	if err != nil {
		return fmt.Sprintf("error saving fact: %v", err)
	}
	label := "user fact"
	if subject == "self" {
		label = "self fact"
	}
	return fmt.Sprintf("saved %s ID=%d: %s", label, id, args.Fact)
}

func execUpdateFact(argsJSON string, store *memory.Store) string {
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

	if err := store.UpdateFact(args.FactID, args.Fact, args.Category, args.Importance); err != nil {
		return fmt.Sprintf("error updating fact: %v", err)
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

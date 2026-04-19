package memory

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	"her/llm"
	"her/logger"
)

// log is the package-level logger for the memory package.
var log = logger.WithPrefix("memory")

// extractionPrompt is loaded from extraction_prompt.md.
// No parameters — used as a static system prompt.
//
// NOTE: ExtractMemories is not currently called anywhere. It's reserved for a
// future "memory pruning" feature where Mira can revisit old conversations
// and re-extract only the memories that held up over time. The agent's
// save_memory tool handles real-time extraction. Keep this prompt in sync
// with the main_agent_prompt.md rules so the two paths agree on what's
// worth saving. Mood extraction lives in the dedicated mood agent (`mood/`),
// not here.
//
//go:embed extraction_prompt.md
var extractionPrompt string

// extractionResponse is the top-level JSON structure from the LLM.
type extractionResponse struct {
	Facts []extractedMemory `json:"facts"`
}

// extractedMemory is the JSON structure for a single memory.
// The JSON tag stays as "fact" to match the LLM response format.
type extractedMemory struct {
	Fact     string `json:"fact"`
	Category string `json:"category"`
}

// ExtractMemories analyzes recent messages and extracts long-term memories
// AND mood data. It sends the conversation to the LLM with an extraction
// prompt, parses the JSON response, and stores each memory + mood in the database.
//
// conversationID identifies which conversation to extract from.
// sinceID is the last message ID that was already extracted — we only
// look at messages after this point to avoid re-extracting.
func ExtractMemories(store *Store, llmClient *llm.Client, conversationID string, sinceID int64, botName, userName string) error {
	// Get the messages we haven't extracted from yet.
	messages, err := store.MessagesAfter(conversationID, sinceID)
	if err != nil {
		return fmt.Errorf("getting messages for extraction: %w", err)
	}

	if len(messages) == 0 {
		return nil
	}

	// Build a conversation transcript for the LLM.
	// We use the raw content here — memory extraction happens locally,
	// and we want full fidelity. The scrubbed version goes to the
	// conversational LLM; extraction sees the real data.
	var transcript strings.Builder
	for _, msg := range messages {
		role := userName
		if msg.Role == "assistant" {
			role = botName
		}
		fmt.Fprintf(&transcript, "%s: %s\n\n", role, msg.ContentRaw)
	}

	// Expand {{her}}/{{user}} placeholders in the prompt template.
	prompt := strings.ReplaceAll(extractionPrompt, "{{her}}", botName)
	prompt = strings.ReplaceAll(prompt, "{{user}}", userName)

	// Call the LLM with the extraction prompt.
	llmMessages := []llm.ChatMessage{
		{Role: "system", Content: prompt},
		{Role: "user", Content: transcript.String()},
	}

	resp, err := llmClient.ChatCompletion(llmMessages)
	if err != nil {
		return fmt.Errorf("LLM extraction call: %w", err)
	}

	// Parse the JSON response. The LLM should return a JSON object,
	// but sometimes it wraps it in markdown code fences. Strip those.
	jsonStr := strings.TrimSpace(resp.Content)
	jsonStr = strings.TrimPrefix(jsonStr, "```json")
	jsonStr = strings.TrimPrefix(jsonStr, "```")
	jsonStr = strings.TrimSuffix(jsonStr, "```")
	jsonStr = strings.TrimSpace(jsonStr)

	var extraction extractionResponse
	if err := json.Unmarshal([]byte(jsonStr), &extraction); err != nil {
		// Fallback: try parsing as a plain memory array (old format).
		// This handles models that don't follow the new schema.
		var memories []extractedMemory
		if err2 := json.Unmarshal([]byte(jsonStr), &memories); err2 == nil {
			extraction.Facts = memories
			log.Debug("extraction response was old-format memory array")
		} else {
			log.Error("failed to parse extraction response as JSON", "err", err, "raw", resp.Content)
			return fmt.Errorf("parsing extraction response: %w", err)
		}
	}

	// Store each extracted memory. Use the last message's ID as the source
	// so we know where extraction left off.
	lastMsgID := messages[len(messages)-1].ID
	for _, m := range extraction.Facts {
		_, err := store.SaveMemory(m.Fact, m.Category, "user", lastMsgID, 5, nil, nil, "", "")
		if err != nil {
			log.Error("saving extracted memory", "err", err)
			continue
		}
		log.Debug("extracted memory", "category", m.Category, "memory", m.Fact)
	}

	log.Info("extraction complete", "memories", len(extraction.Facts), "messages", len(messages))
	return nil
}

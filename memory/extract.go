package memory

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"her-go/llm"
)

// extractionPrompt is the system prompt sent to the LLM to extract facts
// from a conversation. We ask for JSON output so we can parse it reliably.
const extractionPrompt = `You are a memory extraction system. Your job is to read a conversation and extract key facts, events, emotions, and decisions worth remembering long-term.

For each fact, provide:
- "fact": A single clear sentence capturing the information
- "category": One of: relationship, health, work, mood, goal, event, preference, identity, other
- "importance": 1-10 (10 = life-changing event, 1 = trivial detail)

Focus on things that would be useful to remember in future conversations:
- Personal details (name, identity, living situation, relationships)
- Emotional states and patterns
- Goals, plans, and decisions
- Preferences and opinions
- Significant events or changes in their life

Do NOT extract:
- Generic pleasantries ("user said hello")
- Things the assistant said (only extract facts about the user)
- Duplicate information if the same fact appears multiple times

Respond with ONLY a JSON array. No markdown, no code fences, no explanation. Example:
[{"fact": "User's name is Autumn", "category": "identity", "importance": 9}, {"fact": "User recently moved back to parents' house", "category": "event", "importance": 7}]

If there are no meaningful facts to extract, respond with an empty array: []`

// extractedFact is the JSON structure we expect from the LLM.
type extractedFact struct {
	Fact       string `json:"fact"`
	Category   string `json:"category"`
	Importance int    `json:"importance"`
}

// ExtractFacts analyzes recent messages and extracts long-term memory facts.
// It sends the conversation to the LLM with an extraction prompt, parses
// the JSON response, and stores each fact in the database.
//
// conversationID identifies which conversation to extract from.
// sinceID is the last message ID that was already extracted — we only
// look at messages after this point to avoid re-extracting.
// lastMsgID is the ID of the most recent message, used as source_message_id
// for the extracted facts.
func ExtractFacts(store *Store, llmClient *llm.Client, conversationID string, sinceID int64) error {
	// Get the messages we haven't extracted from yet.
	messages, err := store.MessagesAfter(conversationID, sinceID)
	if err != nil {
		return fmt.Errorf("getting messages for extraction: %w", err)
	}

	if len(messages) == 0 {
		return nil
	}

	// Build a conversation transcript for the LLM.
	// We use the raw content here — fact extraction happens locally,
	// and we want full fidelity. The scrubbed version goes to the
	// conversational LLM; extraction sees the real data.
	var transcript strings.Builder
	for _, msg := range messages {
		role := "User"
		if msg.Role == "assistant" {
			role = "Mira"
		}
		fmt.Fprintf(&transcript, "%s: %s\n\n", role, msg.ContentRaw)
	}

	// Call the LLM with the extraction prompt.
	llmMessages := []llm.ChatMessage{
		{Role: "system", Content: extractionPrompt},
		{Role: "user", Content: transcript.String()},
	}

	resp, err := llmClient.ChatCompletion(llmMessages)
	if err != nil {
		return fmt.Errorf("LLM extraction call: %w", err)
	}

	// Parse the JSON response. The LLM should return a JSON array,
	// but sometimes it wraps it in markdown code fences. Strip those.
	jsonStr := strings.TrimSpace(resp.Content)
	jsonStr = strings.TrimPrefix(jsonStr, "```json")
	jsonStr = strings.TrimPrefix(jsonStr, "```")
	jsonStr = strings.TrimSuffix(jsonStr, "```")
	jsonStr = strings.TrimSpace(jsonStr)

	var facts []extractedFact
	if err := json.Unmarshal([]byte(jsonStr), &facts); err != nil {
		log.Printf("Failed to parse extraction response as JSON: %v\nRaw: %s", err, resp.Content)
		return fmt.Errorf("parsing extraction response: %w", err)
	}

	// Store each extracted fact. Use the last message's ID as the source
	// so we know where extraction left off.
	lastMsgID := messages[len(messages)-1].ID
	for _, f := range facts {
		// Clamp importance to valid range.
		if f.Importance < 1 {
			f.Importance = 1
		}
		if f.Importance > 10 {
			f.Importance = 10
		}

		_, err := store.SaveFact(f.Fact, f.Category, "user", lastMsgID, f.Importance, nil)
		if err != nil {
			log.Printf("Error saving extracted fact: %v", err)
			continue
		}
		log.Printf("Extracted fact [%s, importance=%d]: %s", f.Category, f.Importance, f.Fact)
	}

	log.Printf("Extracted %d facts from %d messages", len(facts), len(messages))
	return nil
}

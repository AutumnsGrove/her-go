package memory

import (
	"encoding/json"
	"fmt"
	"strings"

	"her/llm"
	"her/logger"
)

// log is the package-level logger for the memory package.
var log = logger.WithPrefix("memory")

// extractionPrompt is the system prompt sent to the LLM to extract facts
// AND mood from a conversation. We ask for JSON output so we can parse it
// reliably. Mood inference piggybacks on the same LLM call as fact extraction
// to avoid an extra API call -- same conversation, one extra field.
//
// NOTE: ExtractFacts is not currently called anywhere. It's reserved for a
// future "memory pruning" feature where Mira can revisit old conversations
// and re-extract only the facts that held up over time. The agent's save_fact
// tool handles real-time extraction. Keep this prompt in sync with the
// agent_prompt.md rules so the two paths agree on what's worth saving.
const extractionPrompt = `You are a memory extraction system. Your job is to read a conversation and extract two things:

## 1. Facts
Facts worth remembering WEEKS or MONTHS later. Apply the "next month" test: would knowing this fact improve a conversation 30 days from now? If not, skip it.

For each fact, provide:
- "fact": A single clear sentence capturing the information
- "category": One of: identity, relationship, health, work, goal, event, preference, other
- "importance": 1-10 (10 = life-changing event, 1 = trivial detail)

SAVE facts about:
- Personal details (name, identity, living situation, relationships)
- Recurring emotional patterns (not one-off moods)
- Goals, plans, and decisions
- Preferences, opinions, and values that persist over time
- Significant life events or changes

Do NOT extract:
- Generic pleasantries ("user said hello")
- Things the assistant said (only extract facts about the user)
- Duplicate information if the same fact appears multiple times
- Transient moods ("feeling tired", "kind of nothing today", "feeling positive") -- mood tracking handles these separately
- What the user ate, drank, or ordered -- unless it reveals a dietary restriction or lasting pattern
- One-off sensory moments ("saw someone get a latte", "nice hot chocolate")
- Ephemeral daily context ("at coffee shop", "working on X today")
- Vague or trivial observations ("user is feeling positive", "user said something interesting")
- Current tasks or in-progress work details that expire quickly

STYLE RULES for writing facts:
- Each fact must be 1-2 short sentences max. No paragraphs.
- Write like a person jotting a note, not like an essay. Plain and direct.
- NEVER use em dashes. Use periods or commas.
- NEVER use "not just X, it's Y" constructions. Just say Y.
- Avoid grandiose language: "significant moment", "a testament to", "speaks volumes", "deeply personal", "genuinely incredible"
- Avoid corporate filler: "actively investing", "creating a richer", "meta-level", "fundamentally", "remarkably", "transformative"
- Good: "User's dog is named Max. Got him as a puppy last year."
- Bad: "User has a deeply personal bond with their dog Max, who represents not just a pet but a transformative source of companionship."

## 2. Mood
Infer the user's overall emotional state from the conversation.

- "rating": 1-5 scale (1=bad, 2=rough, 3=meh/neutral, 4=good, 5=great)
- "note": Brief description of WHY you rated it this way (1 sentence)
- "tags": Object with optional keys: "energy" (low/medium/high), "stress" (low/medium/high), "social" (isolated/neutral/connected)

Only include mood if you can genuinely infer it from the conversation. If the conversation is purely informational with no emotional signal, set mood to null.

## Response Format

Respond with ONLY a JSON object. No markdown, no code fences, no explanation.

{"facts": [{"fact": "User's name is Autumn", "category": "identity", "importance": 9}], "mood": {"rating": 4, "note": "Seems upbeat, excited about new project", "tags": {"energy": "high", "stress": "low"}}}

If no facts to extract and no mood signal: {"facts": [], "mood": null}`

// extractionResponse is the top-level JSON structure from the LLM.
type extractionResponse struct {
	Facts []extractedFact `json:"facts"`
	Mood  *extractedMood  `json:"mood"` // nil if no mood signal detected
}

// extractedFact is the JSON structure for a single fact.
type extractedFact struct {
	Fact       string `json:"fact"`
	Category   string `json:"category"`
	Importance int    `json:"importance"`
}

// extractedMood is the JSON structure for inferred mood.
type extractedMood struct {
	Rating int               `json:"rating"`
	Note   string            `json:"note"`
	Tags   map[string]string `json:"tags"` // energy, stress, social, etc.
}

// ExtractFacts analyzes recent messages and extracts long-term memory facts
// AND mood data. It sends the conversation to the LLM with an extraction
// prompt, parses the JSON response, and stores each fact + mood in the database.
//
// conversationID identifies which conversation to extract from.
// sinceID is the last message ID that was already extracted — we only
// look at messages after this point to avoid re-extracting.
func ExtractFacts(store *Store, llmClient *llm.Client, conversationID string, sinceID int64, botName, userName string) error {
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
		role := userName
		if msg.Role == "assistant" {
			role = botName
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

	// Parse the JSON response. The LLM should return a JSON object,
	// but sometimes it wraps it in markdown code fences. Strip those.
	jsonStr := strings.TrimSpace(resp.Content)
	jsonStr = strings.TrimPrefix(jsonStr, "```json")
	jsonStr = strings.TrimPrefix(jsonStr, "```")
	jsonStr = strings.TrimSuffix(jsonStr, "```")
	jsonStr = strings.TrimSpace(jsonStr)

	var extraction extractionResponse
	if err := json.Unmarshal([]byte(jsonStr), &extraction); err != nil {
		// Fallback: try parsing as a plain fact array (old format).
		// This handles models that don't follow the new schema.
		var facts []extractedFact
		if err2 := json.Unmarshal([]byte(jsonStr), &facts); err2 == nil {
			extraction.Facts = facts
			log.Debug("extraction response was old-format fact array, no mood data")
		} else {
			log.Error("failed to parse extraction response as JSON", "err", err, "raw", resp.Content)
			return fmt.Errorf("parsing extraction response: %w", err)
		}
	}

	// Store each extracted fact. Use the last message's ID as the source
	// so we know where extraction left off.
	lastMsgID := messages[len(messages)-1].ID
	for _, f := range extraction.Facts {
		// Clamp importance to valid range.
		if f.Importance < 1 {
			f.Importance = 1
		}
		if f.Importance > 10 {
			f.Importance = 10
		}

		_, err := store.SaveFact(f.Fact, f.Category, "user", lastMsgID, f.Importance, nil, nil, "")
		if err != nil {
			log.Error("saving extracted fact", "err", err)
			continue
		}
		log.Debug("extracted fact", "category", f.Category, "importance", f.Importance, "fact", f.Fact)
	}

	// Store inferred mood if the LLM detected an emotional signal.
	if extraction.Mood != nil && extraction.Mood.Rating >= 1 && extraction.Mood.Rating <= 5 {
		tagsJSON := ""
		if len(extraction.Mood.Tags) > 0 {
			if b, err := json.Marshal(extraction.Mood.Tags); err == nil {
				tagsJSON = string(b)
			}
		}
		if _, err := store.SaveMoodEntry(
			extraction.Mood.Rating,
			extraction.Mood.Note,
			tagsJSON,
			"inferred",
			conversationID,
		); err != nil {
			log.Error("saving inferred mood", "err", err)
		} else {
			log.Debug("inferred mood", "rating", extraction.Mood.Rating, "note", extraction.Mood.Note)
		}
	}

	log.Info("extraction complete", "facts", len(extraction.Facts), "messages", len(messages))
	return nil
}

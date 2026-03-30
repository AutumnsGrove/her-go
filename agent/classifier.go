package agent

import (
	"fmt"
	"strings"

	"her/llm"
	"her/memory"
)

// ClassifyVerdict is the result of a classifier check on a proposed
// memory write. The classifier is a small, fast LLM (Haiku-class) that
// evaluates content for multiple quality dimensions: reality vs fiction,
// usefulness, inference vs stated, and proper categorization.
type ClassifyVerdict struct {
	Allowed bool   // true = write should proceed to DB
	Type    string // verdict type: "SAVE", "FICTIONAL", "LOW_VALUE", "MOOD_NOT_FACT", "INFERRED", "EXTERNAL", "HAS_TIMESTAMP"
	Reason  string // human-readable explanation from the classifier
}

// classifyMemoryWrite asks the classifier LLM whether a proposed memory
// write should be saved to the database. It checks for multiple quality
// issues: fictional content, low-value facts, transient moods stored as
// permanent facts, and inferred-not-stated information.
//
// This is the single entry point for all classifier checks. It builds
// the right prompt based on writeType, calls the classifier, and parses
// the response. On ANY error (nil client, LLM failure, unparseable
// response), it returns Allowed=true — fail-open, because a missing
// fact is less harmful than a broken memory system.
//
// writeType: "fact", "self_fact", "mood", or "receipt"
// content: the proposed text (fact text, mood note, or receipt summary)
// snippet: last few messages of conversation for context
func classifyMemoryWrite(
	classifierLLM *llm.Client,
	writeType string,
	content string,
	snippet []memory.Message,
) ClassifyVerdict {
	// Nil-safe: no classifier configured → pass through.
	// This makes the classifier purely opt-in via config.yaml.
	if classifierLLM == nil {
		return ClassifyVerdict{Allowed: true, Type: "SAVE"}
	}

	// Build conversation context from the message snippet.
	// We show the classifier the last few messages so it can tell whether
	// "I got a new sword" is the user talking about a game or real life.
	var contextLines []string
	for _, msg := range snippet {
		// Prefer scrubbed content (PII-safe), fall back to raw.
		text := msg.ContentScrubbed
		if text == "" {
			text = msg.ContentRaw
		}
		contextLines = append(contextLines, fmt.Sprintf("%s: %s", msg.Role, text))
	}
	contextStr := strings.Join(contextLines, "\n")

	// Pick the right system prompt and format the user message.
	var systemPrompt string
	switch writeType {
	case "fact", "self_fact":
		systemPrompt = classifierFactSystem
	case "mood":
		systemPrompt = classifierMoodSystem
	case "receipt":
		systemPrompt = classifierReceiptSystem
	default:
		// Unknown write type → don't block it.
		return ClassifyVerdict{Allowed: true, Type: "SAVE"}
	}

	userPrompt := fmt.Sprintf(
		"Conversation context:\n%s\n\nProposed %s to save:\n%s",
		contextStr, writeType, content,
	)

	messages := []llm.ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}

	resp, err := classifierLLM.ChatCompletion(messages)
	if err != nil {
		// Fail-open: if the classifier is down, let the write through.
		// A missing fact is annoying; blocking ALL memory writes because
		// a safety check is offline would make the bot feel broken.
		log.Warn("classifier LLM failed, allowing write (fail-open)", "err", err, "type", writeType)
		return ClassifyVerdict{Allowed: true, Type: "SAVE"}
	}

	verdict := parseClassifierResponse(resp.Content)
	if !verdict.Allowed {
		log.Info("classifier rejected write", "type", writeType, "verdict", verdict.Type, "reason", verdict.Reason, "content", content)
	}
	return verdict
}

// parseClassifierResponse extracts the verdict from the classifier's
// plain-text response. The first word is the verdict type, everything
// after it on the same line is the reason/explanation.
//
// We use simple string prefix matching rather than JSON parsing because
// small models (Haiku-class) are more reliable with free-form text than
// structured output.
func parseClassifierResponse(response string) ClassifyVerdict {
	line := strings.TrimSpace(response)
	// Take only the first line — the model might add extra explanation.
	if idx := strings.IndexAny(line, "\n\r"); idx >= 0 {
		line = strings.TrimSpace(line[:idx])
	}
	upper := strings.ToUpper(line)

	// --- Allowed verdicts ---
	if strings.HasPrefix(upper, "SAVE") {
		return ClassifyVerdict{Allowed: true, Type: "SAVE"}
	}

	// --- Rejected verdicts ---
	// Each verdict type maps to a specific quality problem. The rejection
	// message in the exec function uses the Type to give the agent
	// actionable feedback (e.g., "use log_mood instead" for MOOD_NOT_FACT).

	if strings.HasPrefix(upper, "FICTIONAL") {
		return ClassifyVerdict{Allowed: false, Type: "FICTIONAL", Reason: extractReason(line)}
	}
	if strings.HasPrefix(upper, "EXTERNAL") {
		return ClassifyVerdict{Allowed: false, Type: "EXTERNAL", Reason: extractReason(line)}
	}
	if strings.HasPrefix(upper, "LOW_VALUE") {
		return ClassifyVerdict{Allowed: false, Type: "LOW_VALUE", Reason: extractReason(line)}
	}
	if strings.HasPrefix(upper, "MOOD_NOT_FACT") {
		return ClassifyVerdict{Allowed: false, Type: "MOOD_NOT_FACT", Reason: extractReason(line)}
	}
	if strings.HasPrefix(upper, "INFERRED") {
		return ClassifyVerdict{Allowed: false, Type: "INFERRED", Reason: extractReason(line)}
	}
	if strings.HasPrefix(upper, "HAS_TIMESTAMP") {
		return ClassifyVerdict{Allowed: false, Type: "HAS_TIMESTAMP", Reason: extractReason(line)}
	}

	// Unparseable response → fail-open.
	log.Warn("classifier returned unparseable response, allowing write", "response", response)
	return ClassifyVerdict{Allowed: true, Type: "SAVE"}
}

// extractReason pulls the explanation text after the verdict keyword.
// "FICTIONAL — this is about a game character" → "this is about a game character"
// "LOW_VALUE" → "" (no explanation given)
func extractReason(line string) string {
	// Strip the verdict keyword (first word).
	parts := strings.SplitN(line, " ", 2)
	if len(parts) < 2 {
		return ""
	}
	reason := strings.TrimSpace(parts[1])
	// Strip leading punctuation that models sometimes add (—, -, :).
	reason = strings.TrimLeft(reason, "—-–: ")
	return reason
}

// rejectionMessage builds the string that gets returned to the agent
// when the classifier rejects a write. The message is tailored to the
// verdict type so the agent knows what to do differently — not just
// "rejected" but "rejected, and here's the right action to take."
func rejectionMessage(verdict ClassifyVerdict) string {
	switch verdict.Type {
	case "FICTIONAL":
		detail := "this describes fictional/in-game content, not the real user"
		if verdict.Reason != "" {
			detail = verdict.Reason
		}
		return fmt.Sprintf("rejected: %s. Only save facts about the real user's actual life.", detail)

	case "LOW_VALUE":
		detail := "this fact is too vague or generic to be worth saving"
		if verdict.Reason != "" {
			detail = verdict.Reason
		}
		return fmt.Sprintf("rejected: %s. Facts should capture specific, meaningful information that couldn't be inferred from conversation history.", detail)

	case "MOOD_NOT_FACT":
		detail := "this is a transient emotional state, not a permanent fact"
		if verdict.Reason != "" {
			detail = verdict.Reason
		}
		return fmt.Sprintf("rejected: %s. Use the log_mood skill instead — it's designed for tracking how the user feels in the moment. Facts should be durable truths, not snapshots of today's mood.", detail)

	case "INFERRED":
		detail := "the user didn't actually state this — the agent is inferring or editorializing"
		if verdict.Reason != "" {
			detail = verdict.Reason
		}
		return fmt.Sprintf("rejected: %s. Only save things the user explicitly said or clearly implied. Don't add interpretations, diagnoses, or pattern analysis.", detail)

	case "EXTERNAL":
		detail := "this mood is about a fictional character, not the real user"
		if verdict.Reason != "" {
			detail = verdict.Reason
		}
		return fmt.Sprintf("rejected: %s. Only log the real user's emotional state.", detail)

	case "HAS_TIMESTAMP":
		detail := "the fact contains a date or time reference"
		if verdict.Reason != "" {
			detail = verdict.Reason
		}
		return fmt.Sprintf("rejected: %s. Timestamps are automatically attached to every fact — do NOT include dates, times, or relative time words (today, yesterday, last week) in the fact text. Rewrite the fact without the temporal reference.", detail)

	default:
		return fmt.Sprintf("rejected by classifier: %s", verdict.Reason)
	}
}

// --- Classifier system prompts ---
//
// These are intentionally direct with concrete examples. Small models
// do better with clear rules and real examples than with nuanced
// instructions. Each write type gets its own prompt so the classifier
// doesn't have to figure out what kind of content it's looking at.

const classifierFactSystem = `You are a quality gate for a personal chatbot's memory system. A fact has been proposed for permanent storage. Evaluate it against ALL of the following criteria and return the FIRST matching verdict.

Check in this order:

1. FICTIONAL — Does the fact describe something that happened to a character in a video game, book, movie, TV show, or roleplay AS IF it happened to the real user?
   Examples of FICTIONAL:
   - "User got a new apartment in Night City" (in-game event)
   - "User pulled out a katana at a festival" (game action)
   - "User prefers spontaneous plans and enjoys pulling out a katana at festivals" (mixes real and fictional)
   Note: discussing fiction is fine. "User enjoys playing Cyberpunk 2077" is SAVE — that's a real preference about the user.

2. MOOD_NOT_FACT — Is this a transient emotional state being stored as a permanent fact? Moods belong in the mood tracker, not the fact database.
   Examples of MOOD_NOT_FACT:
   - "User feels low and frustrated after conflict with parents"
   - "User is feeling kind of nothing today"
   - "User feels overwhelmed and may need a break"
   - "Feeling lonely seeing couples at coffee shop"
   Note: a DURABLE emotional pattern IS a fact. "User experiences recurring episodes of emotional flatness" is SAVE — that's a lasting pattern, not today's mood.

3. INFERRED — Did the user actually say this, or is the chatbot inferring, diagnosing, or editorializing?
   Examples of INFERRED:
   - "User is very self-critical about coping mechanisms" (therapist-style assessment)
   - "User tends to share emotional states early in conversations" (behavioral analysis the user never stated)
   - "User has a toxic relationship with [person]" (judgment call, not stated)
   Note: reasonable summarization is fine. "User is reading a book about addiction" from "I'm reading Never Enough" is SAVE — that's a direct restatement.

4. LOW_VALUE — Is the fact too vague, generic, or obvious to be worth permanent storage? Would it add meaningful context in future conversations?
   Examples of LOW_VALUE:
   - "User enjoys reading and finds it a pleasant activity" (says nothing specific)
   - "User had a conversation today" (trivially obvious)
   - "User sent a message about their day" (meta-observation with no content)
   - "User is interested in technology" (too broad to be useful)
   Note: specificity is what matters. "User enjoys short, surreal books like Piranesi" is SAVE — that's actionable.

5. HAS_TIMESTAMP — Does the fact contain a specific date, time, or relative time reference? Timestamps are automatically attached to every fact by the system. The agent must NOT embed dates into the fact text.
   Examples of HAS_TIMESTAMP:
   - "User visited Zaxby's on March 29" (contains specific date)
   - "As of 2026-03-29, user prefers..." (contains ISO date)
   - "User started therapy last Tuesday" (relative time reference)
   - "User went to a coffee shop today" (relative time — "today")
   - "Yesterday user mentioned feeling better" (relative time — "yesterday")
   Note: recurring schedules and durations are NOT timestamps. "User has therapy on Thursdays" is SAVE — that's a pattern. "User has been learning Go for 3 months" is SAVE — that's a duration, not a date.

6. SAVE — The fact is real, specific, useful, and actually stated or clearly implied by the user.

Respond with exactly one verdict on a single line. Optionally add a brief explanation after the verdict.
Examples: "SAVE", "FICTIONAL — game event from Cyberpunk", "MOOD_NOT_FACT — transient frustration", "LOW_VALUE — too vague to be actionable", "HAS_TIMESTAMP — contains specific date"`

const classifierMoodSystem = `You are a quality gate for a personal chatbot's mood tracker. A mood entry has been proposed. Evaluate it:

1. EXTERNAL — The mood describes how a fictional character feels (in a game, book, movie, show), not the real user.
   Examples of EXTERNAL:
   - "excited about katana build" (game character's excitement)
   - "anticipatory after talking with Wakako" (Wakako is an NPC)
   Note: the user's real emotional REACTION to fiction is SAVE. "Feeling sad about a character's death" is the user's real emotion.

2. SAVE — The mood reflects the real user's actual emotional state.

Respond with exactly one verdict on a single line: SAVE or EXTERNAL`

const classifierReceiptSystem = `You are a quality gate for a personal chatbot's expense tracker. A receipt/expense has been proposed. Evaluate it:

1. FICTIONAL — An in-game purchase, fictional vendor, virtual currency, or game economy event.
   Examples: buying weapons in a game shop, paying an NPC for services, virtual currency transactions.

2. SAVE — A real-world purchase at a real store or service.

Respond with exactly one verdict on a single line: SAVE or FICTIONAL`

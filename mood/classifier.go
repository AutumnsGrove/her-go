package mood

import (
	"context"
	"fmt"
	"strings"

	"her/llm"
)

// classifyReal is a fail-open check that asks a small fast LLM
// (Haiku-class) whether the inference the bigger model produced is
// actually a FIRST-PERSON mood rather than e.g. a fictional
// character's feelings or a description of someone else's state.
//
// Fail-open: on any error — classifier down, network hiccup, parse
// failure — we return (true, ""). A missed classifier pass is
// preferable to blocking real mood writes when a stale API is
// failing. Same policy as the memory classifier.
//
// Returns:
//
//	ok = true:  pass, safe to log
//	ok = false: reject, skip the write with the given reason
func classifyReal(ctx context.Context, client *llm.Client, inf *Inference, turns []Turn) (bool, string) {
	if client == nil || inf == nil {
		return true, ""
	}

	// Trim transcript to a couple of user-side lines — the
	// classifier only needs the user's own words to decide.
	var userLines []string
	for _, t := range turns {
		if t.Role == "user" {
			userLines = append(userLines, t.ScrubbedContent)
		}
	}
	if len(userLines) > 3 {
		userLines = userLines[len(userLines)-3:]
	}
	transcript := strings.Join(userLines, "\n")

	prompt := fmt.Sprintf(classifierPrompt,
		strings.Join(inf.Labels, ", "),
		inf.Note,
		transcript,
	)

	resp, err := client.ChatCompletion([]llm.ChatMessage{
		{Role: "user", Content: prompt},
	})
	if err != nil {
		log.Warn("mood classifier LLM call failed; failing open", "err", err)
		return true, ""
	}
	_ = ctx // reserved for cancellation

	verdict := strings.ToUpper(strings.TrimSpace(resp.Content))
	// Strip any trailing punctuation the model adds.
	verdict = strings.TrimRight(verdict, ".,! ")

	switch {
	case strings.HasPrefix(verdict, "REAL"):
		return true, ""
	case strings.HasPrefix(verdict, "FICTION"),
		strings.HasPrefix(verdict, "NOT_SELF"),
		strings.HasPrefix(verdict, "REJECT"):
		return false, "classifier: " + verdict
	default:
		// Anything else = unparseable → fail open.
		log.Warn("mood classifier returned unexpected verdict; failing open",
			"verdict", verdict)
		return true, ""
	}
}

// classifierPrompt is a dense single-shot prompt. We want a Haiku-
// class model; the cheaper the better. It MUST reply with a single
// word from the allowed set.
const classifierPrompt = `You are a memory-write validator. Decide if the mood observation below is a REAL first-person feeling from the user, FICTION (book/game/movie character), or NOT_SELF (the user describing someone else).

# Inferred mood
Labels: %s
Note: %s

# User's own recent words
%s

# Reply with exactly ONE of:
REAL
FICTION
NOT_SELF`

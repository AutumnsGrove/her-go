package mood

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	"her/llm"
)

// agentPromptTemplate is the system prompt the mood agent uses. It
// embeds the allowed vocabulary at build time — labels and
// associations are injected per-call so the LLM can only pick from
// the current vocab file.
//
// This is a hot-path string: the model sees it with every call. The
// instructions are terse on purpose, because the narrative-language
// models we run here extract structured output well from short
// prompts but drift when
// you bury the schema under prose.
//
// NOTE: this prompt intentionally does NOT include examples. Apple
// State of Mind vocabulary is unambiguous enough that examples can
// skew the model toward whatever example emotions we showed. Ship a
// dedicated eval suite instead.
const agentPromptTemplate = `You are a mood-inference system. Given a recent conversation turn, decide whether the user expressed a mood, and if so, capture it in structured JSON.

# Output (JSON only — no prose, no code fences)
{
  "skip": boolean,
  "reason": string,          // when skip=true
  "valence": int,            // 1..7  (1=very unpleasant, 7=very pleasant)
  "labels": [string],        // pick only from the allowed list below
  "associations": [string],  // pick only from the allowed list below
  "note": string,            // 1 short sentence explaining WHAT you heard
  "confidence": number,      // 0..1
  "signals": [string]        // short substrings in the conversation that led you here
}

# Rules
- skip=true when the user did not express a mood (e.g. asking a factual question, discussing code, referencing fictional characters' feelings).
- valence is required when skip=false.
- Use 1-3 labels; match the valence tier (unpleasant / neutral / pleasant).
- associations are optional; skip them when unsure.
- note quotes or paraphrases from the user's own words; never fabricate.
- confidence reflects how certain you are it's a first-person mood — not how intense the mood is. Explicit affect words → high (0.7+). Inferred from tone → medium (0.4-0.7). Speculative → low (<0.4, set skip=true instead).

# Allowed labels
{{LABELS}}

# Allowed associations
{{ASSOCIATIONS}}

# Conversation
{{TRANSCRIPT}}`

// buildPrompt substitutes the vocab and transcript placeholders in
// the template. Keeps the template a single string constant so
// reviewers can read the whole prompt in one pane.
func buildPrompt(v *Vocab, turns []Turn) string {
	labelList := strings.Join(v.AllLabels(), ", ")
	assocList := strings.Join(v.Associations(), ", ")

	var b strings.Builder
	for _, t := range turns {
		role := "user"
		if t.Role == "assistant" {
			role = "her"
		}
		fmt.Fprintf(&b, "%s: %s\n\n", role, t.ScrubbedContent)
	}

	prompt := agentPromptTemplate
	prompt = strings.ReplaceAll(prompt, "{{LABELS}}", labelList)
	prompt = strings.ReplaceAll(prompt, "{{ASSOCIATIONS}}", assocList)
	prompt = strings.ReplaceAll(prompt, "{{TRANSCRIPT}}", strings.TrimSpace(b.String()))
	return prompt
}

// parseInference decodes the LLM's reply into an Inference. Tolerant
// to markdown code fences around the JSON ("```json\n{...}\n```"),
// and to leading/trailing whitespace the model sometimes adds.
func parseInference(raw string) (*Inference, error) {
	cleaned := strings.TrimSpace(raw)
	cleaned = strings.TrimPrefix(cleaned, "```json")
	cleaned = strings.TrimPrefix(cleaned, "```")
	cleaned = strings.TrimSuffix(cleaned, "```")
	cleaned = strings.TrimSpace(cleaned)

	if cleaned == "" {
		return nil, fmt.Errorf("empty response")
	}

	var inf Inference
	if err := json.Unmarshal([]byte(cleaned), &inf); err != nil {
		return nil, fmt.Errorf("parsing inference JSON: %w", err)
	}
	return &inf, nil
}

// callLLM is the single LLM roundtrip for one mood inference.
// Extracted into its own function so tests can unit-test
// buildPrompt / parseInference separately from the runAgent flow.
func callLLM(ctx context.Context, client *llm.Client, vocab *Vocab, turns []Turn) (*Inference, error) {
	prompt := buildPrompt(vocab, turns)
	resp, err := client.ChatCompletion([]llm.ChatMessage{
		{Role: "user", Content: prompt},
	})
	if err != nil {
		return nil, fmt.Errorf("mood LLM call: %w", err)
	}
	// TODO: pass ctx to ChatCompletion when the client supports context-based cancellation.
	_ = ctx

	inf, err := parseInference(resp.Content)
	if err != nil {
		return nil, err
	}
	return inf, nil
}

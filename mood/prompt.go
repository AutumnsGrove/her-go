package mood

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"her/llm"
)

// defaultMoodPrompt is a minimal fallback if mood_agent_prompt.md can't be
// loaded. The real prompt lives in mood_agent_prompt.md at the project root —
// edit that file to change the prompt without recompiling.
const defaultMoodPrompt = `You are a mood-inference system. Output JSON with: skip, reason, valence (1-7), labels, associations, note, confidence (0-1), signals. Use only labels/associations from the allowed lists. skip=true when no mood expressed.

# Allowed labels
{{LABELS}}

# Allowed associations
{{ASSOCIATIONS}}

# Conversation
{{TRANSCRIPT}}`

// promptFilename is the name of the mood agent prompt file, expected
// to live alongside the other prompt .md files in the project root.
const promptFilename = "mood_agent_prompt.md"

// loadMoodPrompt reads mood_agent_prompt.md from the same directory as
// the main prompt file (cfg.Persona.PromptFile). Falls back to the
// hardcoded default if the file is missing or empty — same pattern as
// the main agent and memory agent prompts.
func loadMoodPrompt(promptDir string) string {
	path := filepath.Join(promptDir, promptFilename)
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		log.Warn("mood prompt file not found, using default", "path", path)
		return defaultMoodPrompt
	}
	return strings.TrimSpace(string(data))
}

// buildPrompt substitutes the vocab and transcript placeholders in
// the loaded template. The template is read from mood_agent_prompt.md
// at call time so edits take effect without recompiling.
func buildPrompt(template string, v *Vocab, turns []Turn) string {
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

	prompt := template
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
// promptDir is the directory containing mood_agent_prompt.md — typically
// the project root (same dir as prompt.md). Empty string uses the default.
func callLLM(ctx context.Context, client *llm.Client, vocab *Vocab, turns []Turn, promptDir string) (*Inference, error) {
	template := loadMoodPrompt(promptDir)
	prompt := buildPrompt(template, vocab, turns)
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

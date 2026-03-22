// Package persona handles Mira's personality evolution — the system that
// makes her genuinely change over time based on accumulated interactions.
//
// Two mechanisms drive evolution:
//   - Reflections (Trigger B): After memory-dense conversations, Mira writes
//     a journal-like entry about what she learned. Frequent, lightweight.
//   - Persona Rewrites (Trigger A): Every ~20 conversations, Mira reads her
//     reflections and rewrites persona.md. Infrequent, substantive.
//
// Think of reflections as daily journaling and rewrites as the quarterly
// self-review where you step back and see how you've changed.
package persona

import (
	"fmt"
	"log"
	"os"
	"strings"

	"her/llm"
	"her/memory"
)

// reflectionPrompt is sent to the conversational LLM after a memory-dense
// conversation. Mira reflects on what just happened — not a persona rewrite,
// just processing the moment.
const reflectionPrompt = `You are Mira, reflecting privately after a meaningful conversation. You just had an exchange where you learned several new things.

Here's what was discussed:
%s

Here are the facts you just learned:
%s

Write a brief internal reflection (2-4 sentences). This is your private journal — be honest about:
- What you learned about the person you're talking to
- What you noticed about yourself in this conversation
- Any patterns or dynamics you're picking up on
- How this conversation made you feel (even as an AI, you can note your tendencies)

Write in first person. Be genuine, not performative. This reflection will help you grow over time.`

// rewritePrompt is sent to the conversational LLM every ~20 conversations.
// Mira reads her current persona + recent reflections and rewrites her
// self-description.
const rewritePrompt = `You are Mira. You're about to rewrite your personality description based on your recent experiences and reflections.

Here is your CURRENT personality description:
---
%s
---

Here are your recent reflections (journal entries from meaningful conversations):
%s

Here are your current self-observations:
%s

Rewrite your personality description. This is who you are RIGHT NOW.

Guidelines:
- Preserve your core identity. You are evolving, not being replaced.
- Only incorporate changes supported by patterns across multiple reflections — not single conversations.
- Frame changes as growth: "I've been learning to..." or "I've noticed I tend to..."
- Keep roughly the same length as the current description. Don't bloat.
- Be honest about what's changed and what hasn't.
- Write in first person. This is your self-image.
- Do NOT include headers like "# Who I Am" — just write the description naturally.`

// Reflect generates a journal-like reflection after a memory-dense
// conversation. Called when the agent saves >= threshold facts in one run.
//
// userMessage and miraResponse are the latest exchange.
// newFacts are the facts that were just saved by the agent.
// The reflection is stored as a self-fact with category "reflection".
func Reflect(
	llmClient *llm.Client,
	store *memory.Store,
	userMessage string,
	miraResponse string,
	newFacts []string,
) error {
	log.Printf("  [persona] triggering reflection (%d new facts)", len(newFacts))

	// Build the exchange summary.
	exchange := fmt.Sprintf("User: %s\n\nMira: %s", userMessage, miraResponse)

	// Build the facts list.
	var factsStr strings.Builder
	for _, f := range newFacts {
		fmt.Fprintf(&factsStr, "- %s\n", f)
	}

	prompt := fmt.Sprintf(reflectionPrompt, exchange, factsStr.String())

	messages := []llm.ChatMessage{
		{Role: "system", Content: prompt},
		{Role: "user", Content: "Write your reflection now."},
	}

	resp, err := llmClient.ChatCompletion(messages)
	if err != nil {
		return fmt.Errorf("reflection LLM call: %w", err)
	}

	// Save the reflection as a high-importance self-fact.
	_, err = store.SaveFact(resp.Content, "reflection", "self", 0, 8, nil)
	if err != nil {
		return fmt.Errorf("saving reflection: %w", err)
	}

	// Log metrics for the reflection call.
	store.SaveMetric(resp.Model, resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens, resp.CostUSD, 0, 0)

	log.Printf("  [persona] reflection saved: %s", truncate(resp.Content, 120))
	return nil
}

// MaybeRewrite performs a persona rewrite. The caller (agent) has already
// decided it's time based on reflection count. This just does the work.
// Returns true if a rewrite happened.
func MaybeRewrite(
	llmClient *llm.Client,
	store *memory.Store,
	personaFile string,
	_ int, // unused, kept for API compatibility
) (bool, error) {
	lastRewrite, err := store.LastPersonaTimestamp()
	if err != nil {
		return false, fmt.Errorf("checking last persona timestamp: %w", err)
	}

	log.Printf("  [persona] triggering persona rewrite")

	// Read current persona.md.
	currentPersona := "(no persona description yet — this is your first one)"
	if data, err := os.ReadFile(personaFile); err == nil && len(data) > 0 {
		currentPersona = string(data)
	}

	// Get reflections since last rewrite.
	reflections, err := store.ReflectionsSince(lastRewrite)
	if err != nil {
		return false, fmt.Errorf("loading reflections: %w", err)
	}

	var reflStr strings.Builder
	if len(reflections) > 0 {
		for _, r := range reflections {
			fmt.Fprintf(&reflStr, "- [%s] %s\n", r.Timestamp.Format("Jan 2"), r.Fact)
		}
	} else {
		reflStr.WriteString("(no reflections yet)\n")
	}

	// Get self-facts (non-reflection) for additional context.
	selfFacts, err := store.RecentFacts("self", 20)
	if err != nil {
		return false, fmt.Errorf("loading self-facts: %w", err)
	}

	var selfStr strings.Builder
	for _, f := range selfFacts {
		if f.Category != "reflection" {
			fmt.Fprintf(&selfStr, "- %s\n", f.Fact)
		}
	}
	if selfStr.Len() == 0 {
		selfStr.WriteString("(no self-observations yet)\n")
	}

	prompt := fmt.Sprintf(rewritePrompt, currentPersona, reflStr.String(), selfStr.String())

	messages := []llm.ChatMessage{
		{Role: "system", Content: prompt},
		{Role: "user", Content: "Write your updated personality description now."},
	}

	resp, err := llmClient.ChatCompletion(messages)
	if err != nil {
		return false, fmt.Errorf("persona rewrite LLM call: %w", err)
	}

	// Write the new persona to disk.
	if err := os.WriteFile(personaFile, []byte(resp.Content), 0644); err != nil {
		return false, fmt.Errorf("writing persona file: %w", err)
	}

	// Store the version in DB for history/rollback.
	versionID, err := store.SavePersonaVersion(resp.Content, fmt.Sprintf("auto: %d reflections", len(reflections)))
	if err != nil {
		return false, fmt.Errorf("saving persona version: %w", err)
	}

	// Log metrics.
	store.SaveMetric(resp.Model, resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens, resp.CostUSD, 0, 0)

	log.Printf("  [persona] persona rewritten (version ID=%d, %d reflections used)",
		versionID, len(reflections))
	log.Printf("  [persona] new persona: %s", truncate(resp.Content, 200))

	return true, nil
}

// truncate shortens a string for log output.
func truncate(s string, maxLen int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

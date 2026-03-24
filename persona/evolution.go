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
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"

	"her/llm"
	"her/logger"
	"her/memory"
)

// log is the package-level logger for the persona package.
var log = logger.WithPrefix("persona")

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
// The reflection is stored in the dedicated reflections table via SaveReflection.
func Reflect(
	llmClient *llm.Client,
	store *memory.Store,
	userMessage string,
	miraResponse string,
	newFacts []string,
) error {
	log.Info("triggering reflection", "new_facts", len(newFacts))

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

	// Save the reflection to its dedicated table.
	// Reflections are private processing — they're separate from the facts
	// table so they don't pollute the user-facing memory context.
	_, err = store.SaveReflection(resp.Content, len(newFacts), userMessage, miraResponse)
	if err != nil {
		return fmt.Errorf("saving reflection: %w", err)
	}

	// Log metrics for the reflection call.
	store.SaveMetric(resp.Model, resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens, resp.CostUSD, 0, 0)

	log.Info("reflection saved", "preview", truncate(resp.Content, 120))
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

	log.Info("triggering persona rewrite")

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
			// r is now a memory.Reflection, so we use r.Content instead of r.Fact.
			fmt.Fprintf(&reflStr, "- [%s] %s\n", r.Timestamp.Format("Jan 2"), r.Content)
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
		// Reflections no longer appear in the facts table, so no
		// category filter is needed here — every self-fact is a real observation.
		fmt.Fprintf(&selfStr, "- %s\n", f.Fact)
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

	log.Info("persona rewritten", "version_id", versionID, "reflections_used", len(reflections))
	log.Info("new persona preview", "preview", truncate(resp.Content, 200))

	// Extract and save trait scores for this persona version.
	// Runs after the rewrite so it doesn't slow down the response pipeline.
	if err := ExtractTraits(llmClient, store, resp.Content, versionID, 0.1); err != nil {
		log.Error("trait extraction failed", "err", err)
		// Non-fatal — persona rewrite still succeeded.
	}

	return true, nil
}

// traitExtractionPrompt asks the LLM to score personality traits based
// on the persona text. Returns JSON so we can parse it programmatically.
const traitExtractionPrompt = `Score these personality traits based on the persona description below.
Return ONLY valid JSON, no other text: {"warmth": 0.7, "directness": 0.5, "humor_style": "dry", "initiative": 0.4, "depth": 0.6}

Trait definitions:
- warmth (0.0-1.0): 0.0 = cold/reserved, 1.0 = deeply warm/emotionally present
- directness (0.0-1.0): 0.0 = very diplomatic/indirect, 1.0 = blunt/straightforward
- humor_style (one of: dry, playful, sardonic, warm, deadpan): the dominant humor type
- initiative (0.0-1.0): 0.0 = purely reactive/follows, 1.0 = proactively leads conversations
- depth (0.0-1.0): 0.0 = keeps things light/casual, 1.0 = tends toward deep/philosophical

Persona description:
---
%s
---

%s`

// ExtractTraits asks the LLM to score personality traits from a persona
// description, applies damping to prevent wild swings, and saves the
// results linked to the persona version.
//
// maxShift caps how much any numeric trait can change per rewrite cycle
// (default 0.1). humor_style is categorical — no damping needed.
func ExtractTraits(
	llmClient *llm.Client,
	store *memory.Store,
	personaText string,
	personaVersionID int64,
	maxShift float64,
) error {
	// Get previous traits for continuity context and damping.
	prevTraits, err := store.GetCurrentTraits()
	if err != nil {
		log.Warn("couldn't load previous traits for damping", "err", err)
	}

	prevContext := "Previous trait scores: none yet (first scoring)"
	prevMap := make(map[string]string)
	if len(prevTraits) > 0 {
		var parts []string
		for _, t := range prevTraits {
			parts = append(parts, fmt.Sprintf("%s=%s", t.TraitName, t.Value))
			prevMap[t.TraitName] = t.Value
		}
		prevContext = fmt.Sprintf("Previous trait scores (shift gradually): %s", strings.Join(parts, ", "))
	}

	prompt := fmt.Sprintf(traitExtractionPrompt, personaText, prevContext)

	messages := []llm.ChatMessage{
		{Role: "system", Content: prompt},
		{Role: "user", Content: "Score the traits now. Return only JSON."},
	}

	resp, err := llmClient.ChatCompletion(messages)
	if err != nil {
		return fmt.Errorf("trait extraction LLM call: %w", err)
	}

	store.SaveMetric(resp.Model, resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens, resp.CostUSD, 0, 0)

	// Parse the JSON response.
	var scores struct {
		Warmth     float64 `json:"warmth"`
		Directness float64 `json:"directness"`
		HumorStyle string  `json:"humor_style"`
		Initiative float64 `json:"initiative"`
		Depth      float64 `json:"depth"`
	}

	// The LLM might wrap JSON in markdown code fences — strip them.
	cleaned := strings.TrimSpace(resp.Content)
	cleaned = strings.TrimPrefix(cleaned, "```json")
	cleaned = strings.TrimPrefix(cleaned, "```")
	cleaned = strings.TrimSuffix(cleaned, "```")
	cleaned = strings.TrimSpace(cleaned)

	if err := json.Unmarshal([]byte(cleaned), &scores); err != nil {
		return fmt.Errorf("parsing trait scores JSON: %w (raw: %s)", err, truncate(resp.Content, 100))
	}

	// Apply damping — clamp numeric traits to ±maxShift from previous.
	scores.Warmth = dampTrait(scores.Warmth, prevMap["warmth"], maxShift)
	scores.Directness = dampTrait(scores.Directness, prevMap["directness"], maxShift)
	scores.Initiative = dampTrait(scores.Initiative, prevMap["initiative"], maxShift)
	scores.Depth = dampTrait(scores.Depth, prevMap["depth"], maxShift)

	// Validate humor_style.
	validHumor := map[string]bool{"dry": true, "playful": true, "sardonic": true, "warm": true, "deadpan": true}
	if !validHumor[scores.HumorStyle] {
		scores.HumorStyle = "dry" // safe default
	}

	// Build trait records and save.
	traits := []memory.Trait{
		{TraitName: "warmth", Value: fmt.Sprintf("%.2f", scores.Warmth)},
		{TraitName: "directness", Value: fmt.Sprintf("%.2f", scores.Directness)},
		{TraitName: "humor_style", Value: scores.HumorStyle},
		{TraitName: "initiative", Value: fmt.Sprintf("%.2f", scores.Initiative)},
		{TraitName: "depth", Value: fmt.Sprintf("%.2f", scores.Depth)},
	}

	if err := store.SaveTraits(traits, personaVersionID); err != nil {
		return fmt.Errorf("saving traits: %w", err)
	}

	log.Info("traits extracted",
		"warmth", scores.Warmth,
		"directness", scores.Directness,
		"humor", scores.HumorStyle,
		"initiative", scores.Initiative,
		"depth", scores.Depth,
	)
	return nil
}

// dampTrait clamps a new trait value to within ±maxShift of the previous
// value. If there's no previous value, the new value is used as-is.
// All numeric traits are clamped to 0.0–1.0.
func dampTrait(newVal float64, prevStr string, maxShift float64) float64 {
	// Clamp to valid range first.
	newVal = math.Max(0, math.Min(1, newVal))

	if prevStr == "" {
		return newVal // no previous, use raw
	}

	prev, err := strconv.ParseFloat(prevStr, 64)
	if err != nil {
		return newVal // can't parse previous, use raw
	}

	// Clamp the delta.
	delta := newVal - prev
	if delta > maxShift {
		delta = maxShift
	} else if delta < -maxShift {
		delta = -maxShift
	}

	return math.Max(0, math.Min(1, prev+delta))
}

// truncate shortens a string for log output.
func truncate(s string, maxLen int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

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
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"her/config"
	"her/llm"
	"her/logger"
	"her/memory"
)

// log is the package-level logger for the persona package.
var log = logger.WithPrefix("persona")

// reflectionPromptTmpl is loaded from reflection_prompt.md.
// Parameters (in order): botName, exchange, facts.
//
//go:embed reflection_prompt.md
var reflectionPromptTmpl string

// rewritePromptTmpl is loaded from rewrite_prompt.md.
// Parameters (in order): botName, currentPersona, reflections, selfFacts.
//
//go:embed rewrite_prompt.md
var rewritePromptTmpl string

// Reflect generates a journal-like reflection after a memory-dense
// conversation. Called when the agent saves >= threshold facts in one run.
//
// userMessage and miraResponse are the latest exchange.
// newFacts are the facts that were just saved by the agent.
// The reflection is stored in the dedicated reflections table via SaveReflection.
func Reflect(
	llmClient *llm.Client,
	store memory.Store,
	userMessage string,
	botResponse string,
	newFacts []string,
	botName, userName string,
) error {
	log.Info("triggering reflection", "new_facts", len(newFacts))

	// Build the exchange summary using configured names.
	exchange := fmt.Sprintf("%s: %s\n\n%s: %s", userName, userMessage, botName, botResponse)

	// Build the facts list.
	var factsStr strings.Builder
	for _, f := range newFacts {
		fmt.Fprintf(&factsStr, "- %s\n", f)
	}

	prompt := fmt.Sprintf(reflectionPromptTmpl, botName, exchange, factsStr.String())

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
	_, err = store.SaveReflection(resp.Content, len(newFacts), userMessage, botResponse)
	if err != nil {
		return fmt.Errorf("saving reflection: %w", err)
	}

	// Log metrics for the reflection call.
	store.SaveMetric(resp.Model, resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens, resp.CostUSD, 0, 0, resp.UsedFallback)

	log.Info("reflection saved", "preview", truncate(resp.Content, 120))
	return nil
}

// MaybeRewrite performs a persona rewrite. The caller (agent) has already
// decided it's time based on reflection count. This just does the work.
// Returns true if a rewrite happened.
func MaybeRewrite(
	llmClient *llm.Client,
	store memory.Store,
	personaFile string,
	_ int, // unused, kept for API compatibility
	botName string,
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
	selfFacts, err := store.RecentMemories("self", 20)
	if err != nil {
		return false, fmt.Errorf("loading self-facts: %w", err)
	}

	var selfStr strings.Builder
	for _, f := range selfFacts {
		// Reflections no longer appear in the facts table, so no
		// category filter is needed here — every self-fact is a real observation.
		fmt.Fprintf(&selfStr, "- %s\n", f.Content)
	}
	if selfStr.Len() == 0 {
		selfStr.WriteString("(no self-observations yet)\n")
	}

	prompt := fmt.Sprintf(rewritePromptTmpl, botName, currentPersona, reflStr.String(), selfStr.String())

	messages := []llm.ChatMessage{
		{Role: "system", Content: prompt},
		{Role: "user", Content: "Write your updated personality description now."},
	}

	resp, err := llmClient.ChatCompletion(messages)
	if err != nil {
		return false, fmt.Errorf("persona rewrite LLM call: %w", err)
	}

	// Swap the bot's literal name back to {{her}} before writing to disk.
	// The LLM writes naturally ("I'm Mira...") because we expanded the
	// template before it saw the prompt. Re-templating here ensures the
	// file stays portable — a fork that changes the name won't inherit
	// a hardcoded "Mira" in the persona.
	personaContent := strings.ReplaceAll(resp.Content, botName, "{{her}}")

	// Write the new persona to disk.
	if err := os.WriteFile(personaFile, []byte(personaContent), 0644); err != nil {
		return false, fmt.Errorf("writing persona file: %w", err)
	}

	// Store the version in DB for history/rollback (raw LLM output,
	// not the templated version — the DB records what the LLM actually said).
	versionID, err := store.SavePersonaVersion(resp.Content, fmt.Sprintf("auto: %d reflections", len(reflections)))
	if err != nil {
		return false, fmt.Errorf("saving persona version: %w", err)
	}

	// Log metrics.
	store.SaveMetric(resp.Model, resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens, resp.CostUSD, 0, 0, resp.UsedFallback)

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

// traitExtractionPrompt is loaded from trait_extraction_prompt.md.
// Parameters (in order): personaText, prevContext.
//
//go:embed trait_extraction_prompt.md
var traitExtractionPrompt string

// ExtractTraits asks the LLM to score personality traits from a persona
// description, applies damping to prevent wild swings, and saves the
// results linked to the persona version.
//
// maxShift caps how much any numeric trait can change per rewrite cycle
// (default 0.1). humor_style is categorical — no damping needed.
func ExtractTraits(
	llmClient *llm.Client,
	store memory.Store,
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

	store.SaveMetric(resp.Model, resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens, resp.CostUSD, 0, 0, resp.UsedFallback)

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

// nightlyReflectPromptTmpl is loaded from nightly_reflect_prompt.md.
// Parameters (in order): botName, currentPersona, traitSummary, recentConvo, userFacts.
//
//go:embed nightly_reflect_prompt.md
var nightlyReflectPromptTmpl string

// gatedRewritePromptTmpl is loaded from gated_rewrite_prompt.md.
// Parameters (in order): botName, currentPersona, reflections, selfFacts.
//
//go:embed gated_rewrite_prompt.md
var gatedRewritePromptTmpl string

// NightlyReflect runs the dreaming system's reflection step. Unlike Reflect()
// (which is triggered by fact density during a turn), this is time-triggered
// and introspective — it looks at the bot's current persona, recent traits,
// and recent conversation to produce a holistic observation.
//
// If the LLM returns "NOTHING_NOTABLE", no reflection is saved (this is expected
// on quiet days). Otherwise the reflection is saved and the dreaming timestamp
// is updated.
func NightlyReflect(
	llmClient *llm.Client,
	store memory.Store,
	cfg *config.Config,
	botName, userName string,
) error {
	log.Info("nightly reflection starting")

	// Read current persona as an anchor.
	currentPersona := "(no persona description yet)"
	if data, err := os.ReadFile(cfg.Persona.PersonaFile); err == nil && len(data) > 0 {
		currentPersona = string(data)
	}

	// Current trait scores — the primary signal for the reflection.
	traits, _ := store.GetCurrentTraits()
	var traitStr strings.Builder
	if len(traits) == 0 {
		traitStr.WriteString("(no trait scores yet)")
	} else {
		for _, t := range traits {
			fmt.Fprintf(&traitStr, "- %s: %s\n", t.TraitName, t.Value)
		}
	}

	// Recent conversation for context (secondary signal).
	recent, _ := store.GlobalRecentMessages(20)
	var convoStr strings.Builder
	if len(recent) == 0 {
		convoStr.WriteString("(no recent messages)")
	} else {
		for _, m := range recent {
			role := userName
			if m.Role == "assistant" {
				role = botName
			}
			content := m.ContentRaw
			if len(content) > 200 {
				content = content[:200] + "..."
			}
			fmt.Fprintf(&convoStr, "%s: %s\n", role, content)
		}
	}

	// Recent user facts (light context — this reflection is about the bot, not the user).
	userFacts, _ := store.RecentMemories("user", 10)
	var userFactStr strings.Builder
	if len(userFacts) == 0 {
		userFactStr.WriteString("(no user facts yet)")
	} else {
		for _, f := range userFacts {
			fmt.Fprintf(&userFactStr, "- %s\n", f.Content)
		}
	}

	prompt := fmt.Sprintf(nightlyReflectPromptTmpl,
		botName,
		currentPersona,
		traitStr.String(),
		convoStr.String(),
		userFactStr.String(),
	)

	messages := []llm.ChatMessage{
		{Role: "system", Content: prompt},
		{Role: "user", Content: "Write your reflection now."},
	}

	resp, err := llmClient.ChatCompletion(messages)
	if err != nil {
		return fmt.Errorf("nightly reflection LLM call: %w", err)
	}

	store.SaveMetric(resp.Model, resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens, resp.CostUSD, 0, 0, resp.UsedFallback)

	content := strings.TrimSpace(resp.Content)
	if content == "NOTHING_NOTABLE" {
		log.Info("nightly reflection: nothing notable, skipping save")
		// Still update the timestamp so the dreamer knows it ran.
		store.SetLastReflectionAt(time.Now())
		return nil
	}

	// Save the reflection and record the timestamp.
	if _, err := store.SaveReflection(content, 0, "", ""); err != nil {
		return fmt.Errorf("saving nightly reflection: %w", err)
	}
	if err := store.SetLastReflectionAt(time.Now()); err != nil {
		log.Warn("failed to update last_reflection_at", "err", err)
	}

	log.Info("nightly reflection saved", "preview", truncate(content, 120))
	return nil
}

// GatedRewrite runs the dreaming system's rewrite step. Two gates must both pass
// before a rewrite is attempted (unless bypass is true, which is used by /dream
// and the sim run_dream flag):
//
//  1. daysSinceLastRewrite >= minRewriteDays (default 7)
//  2. unconsumedReflectionCount >= minReflections (default 3)
//
// The LLM may still return UNCHANGED even when gates pass, if it decides nothing
// substantial has shifted. Returns (true, nil) when the persona was rewritten,
// (false, nil) when gates blocked or LLM returned UNCHANGED.
func GatedRewrite(
	llmClient *llm.Client,
	store memory.Store,
	personaFile string,
	botName string,
	bypass bool,
	minRewriteDays int,
	minReflections int,
) (bool, error) {
	if !bypass {
		state, err := store.GetPersonaState()
		if err != nil {
			return false, fmt.Errorf("reading persona state: %w", err)
		}

		// Gate 1: minimum days since last rewrite.
		if !state.LastRewriteAt.IsZero() {
			daysSince := time.Since(state.LastRewriteAt).Hours() / 24
			if daysSince < float64(minRewriteDays) {
				log.Info("dream rewrite gated: too soon", "days_since", daysSince, "min", minRewriteDays)
				return false, nil
			}
		}

		// Gate 2: minimum unconsumed reflections.
		unconsumed, err := store.UnconsumedReflectionCount()
		if err != nil {
			return false, fmt.Errorf("counting unconsumed reflections: %w", err)
		}
		if unconsumed < minReflections {
			log.Info("dream rewrite gated: not enough reflections", "unconsumed", unconsumed, "min", minReflections)
			return false, nil
		}

		log.Info("dream rewrite gates passed", "unconsumed_reflections", unconsumed)
	} else {
		log.Info("dream rewrite: bypass mode, skipping gates")
	}

	// Read current persona.
	currentPersona := "(no persona description yet — this is your first one)"
	if data, err := os.ReadFile(personaFile); err == nil && len(data) > 0 {
		currentPersona = string(data)
	}

	// Get all unconsumed reflections.
	state, _ := store.GetPersonaState()
	reflections, err := store.ReflectionsSince(state.LastRewriteAt)
	if err != nil {
		return false, fmt.Errorf("loading reflections: %w", err)
	}

	var reflStr strings.Builder
	if len(reflections) == 0 {
		reflStr.WriteString("(no reflections yet)\n")
	} else {
		for _, r := range reflections {
			fmt.Fprintf(&reflStr, "- [%s] %s\n", r.Timestamp.Format("Jan 2"), r.Content)
		}
	}

	// Self-facts for additional context.
	selfFacts, err := store.RecentMemories("self", 20)
	if err != nil {
		return false, fmt.Errorf("loading self-facts: %w", err)
	}

	var selfStr strings.Builder
	for _, f := range selfFacts {
		fmt.Fprintf(&selfStr, "- %s\n", f.Content)
	}
	if selfStr.Len() == 0 {
		selfStr.WriteString("(no self-observations yet)\n")
	}

	prompt := fmt.Sprintf(gatedRewritePromptTmpl, botName, currentPersona, reflStr.String(), selfStr.String())

	messages := []llm.ChatMessage{
		{Role: "system", Content: prompt},
		{Role: "user", Content: "Review your reflections and update your description if warranted."},
	}

	resp, err := llmClient.ChatCompletion(messages)
	if err != nil {
		return false, fmt.Errorf("gated rewrite LLM call: %w", err)
	}

	store.SaveMetric(resp.Model, resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens, resp.CostUSD, 0, 0, resp.UsedFallback)

	content := strings.TrimSpace(resp.Content)
	if strings.HasPrefix(content, "UNCHANGED") {
		log.Info("dream rewrite: LLM returned UNCHANGED, persona stable")
		// Update the rewrite timestamp so the gate resets — the LLM made a
		// deliberate decision, which counts as a "rewrite cycle" for gating purposes.
		store.SetLastRewriteAt(time.Now())
		return false, nil
	}

	// Parse CHANGE_SUMMARY: ... \n---\n <new persona>
	// The expected format is:
	//   CHANGE_SUMMARY: <one sentence>
	//   ---
	//   <new persona text>
	//
	// LLMs sometimes skip the "---" separator and just use a blank line, so we
	// handle both: split on "\n---\n" first, then fall back to stripping the
	// CHANGE_SUMMARY line directly. Either way, the summary line must never
	// end up inside persona.md.
	parts := strings.SplitN(content, "\n---\n", 2)
	var newPersona string
	if len(parts) == 2 {
		// Happy path: model followed the exact format.
		summaryLine := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(parts[0]), "CHANGE_SUMMARY:"))
		newPersona = strings.TrimSpace(parts[1])
		log.Info("dream rewrite: persona change", "summary", summaryLine)
	} else if strings.HasPrefix(content, "CHANGE_SUMMARY:") {
		// Model included the summary line but omitted the "---" separator.
		// Strip the first line and use the rest as the persona body.
		firstNewline := strings.Index(content, "\n")
		if firstNewline != -1 {
			summaryLine := strings.TrimSpace(strings.TrimPrefix(content[:firstNewline], "CHANGE_SUMMARY:"))
			newPersona = strings.TrimSpace(content[firstNewline+1:])
			log.Warn("dream rewrite: missing '---' separator, stripped CHANGE_SUMMARY line", "summary", summaryLine)
		} else {
			// Entire response is just the CHANGE_SUMMARY line with no body — unusable.
			return false, fmt.Errorf("gated rewrite: response has CHANGE_SUMMARY but no persona body")
		}
	} else {
		// No recognisable structure — use the full response as-is and warn loudly.
		newPersona = content
		log.Warn("dream rewrite: response didn't match CHANGE_SUMMARY format, using full content")
	}

	if newPersona == "" {
		return false, fmt.Errorf("gated rewrite: parsed empty persona")
	}

	// Re-template the bot name back to {{her}} so the file stays portable.
	personaContent := strings.ReplaceAll(newPersona, botName, "{{her}}")

	if err := os.WriteFile(personaFile, []byte(personaContent), 0644); err != nil {
		return false, fmt.Errorf("writing persona file: %w", err)
	}

	versionID, err := store.SavePersonaVersion(newPersona, fmt.Sprintf("dream: %d reflections", len(reflections)))
	if err != nil {
		return false, fmt.Errorf("saving persona version: %w", err)
	}

	if err := store.SetLastRewriteAt(time.Now()); err != nil {
		log.Warn("failed to update last_rewrite_at", "err", err)
	}

	store.SaveMetric(resp.Model, resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens, resp.CostUSD, 0, 0, resp.UsedFallback)
	log.Info("dream rewrite complete", "version_id", versionID, "reflections_used", len(reflections))

	// Extract and save trait scores for the new persona version.
	if err := ExtractTraits(llmClient, store, newPersona, versionID, 0.1); err != nil {
		log.Error("trait extraction after dream rewrite failed", "err", err)
		// Non-fatal — the rewrite succeeded.
	}

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

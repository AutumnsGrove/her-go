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
	"her/embed"
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

// analysisPromptTmpl is Step 1 of the two-step rewrite: the LLM distills
// reflections + traits + self-facts into 3-5 factual bullet points.
// Parameters (in order): botName, traitSummary, reflections, selfFacts.
//
//go:embed persona_analysis_prompt.md
var analysisPromptTmpl string

// composePromptTmpl is Step 2: the LLM writes the persona from bullet points.
// Parameters (in order): botName, bullets, warmth, directness, humor, initiative, depth.
//
//go:embed persona_compose_prompt.md
var composePromptTmpl string

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

// MaybeRewrite performs a persona rewrite using the two-step analysis→compose
// pipeline. The caller has already decided it's time. Returns true if a rewrite happened.
func MaybeRewrite(
	llmClient *llm.Client,
	embedClient *embed.Client,
	store memory.Store,
	personaFile string,
	botName string,
) (bool, error) {
	lastRewrite, err := store.LastPersonaTimestamp()
	if err != nil {
		return false, fmt.Errorf("checking last persona timestamp: %w", err)
	}

	log.Info("triggering persona rewrite")

	reflections, err := store.ReflectionsSince(lastRewrite)
	if err != nil {
		return false, fmt.Errorf("loading reflections: %w", err)
	}

	newPersona, _, changed, err := twoStepRewrite(llmClient, embedClient, store, reflections, botName)
	if err != nil {
		return false, err
	}
	if !changed {
		return false, nil
	}

	return commitPersona(llmClient, store, personaFile, newPersona, botName, fmt.Sprintf("auto: %d reflections", len(reflections)))
}

// commitPersona writes the new persona to disk, saves a version in the DB,
// and runs trait extraction. Shared by MaybeRewrite and GatedRewrite.
func commitPersona(
	llmClient *llm.Client,
	store memory.Store,
	personaFile, newPersona, botName, trigger string,
) (bool, error) {
	// Re-template the bot name back to {{her}} so the file stays portable.
	personaContent := strings.ReplaceAll(newPersona, botName, "{{her}}")

	if err := os.WriteFile(personaFile, []byte(personaContent), 0644); err != nil {
		return false, fmt.Errorf("writing persona file: %w", err)
	}

	versionID, err := store.SavePersonaVersion(newPersona, trigger)
	if err != nil {
		return false, fmt.Errorf("saving persona version: %w", err)
	}

	log.Info("persona rewritten", "version_id", versionID, "trigger", trigger)
	log.Info("new persona preview", "preview", truncate(newPersona, 200))

	if err := ExtractTraits(llmClient, store, newPersona, versionID, 0.1); err != nil {
		log.Error("trait extraction failed", "err", err)
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
// Parameters (in order): botName, currentPersona, traitSummary, recentConvo, userFacts, recentReflections.
//
//go:embed nightly_reflect_prompt.md
var nightlyReflectPromptTmpl string

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

	// Use the message watermark to fetch only messages the dreamer hasn't
	// seen yet. This is the structural fix for duplicate reflections on
	// quiet days — if there are no new messages, we skip the LLM call entirely.
	state, _ := store.GetPersonaState()
	recent, _ := store.MessagesAfterID(state.LastReflectedMessageID, 20)

	if len(recent) == 0 {
		log.Info("nightly reflection: no new messages since last reflection, skipping")
		store.SetLastReflectionAt(time.Now())
		return nil
	}

	// Advance watermark to the highest message ID we're about to reflect on.
	maxID := recent[len(recent)-1].ID
	defer func() {
		if err := store.SetLastReflectedMessageID(maxID); err != nil {
			log.Warn("failed to update message watermark", "err", err)
		}
	}()

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

	// Format the new messages as conversation context.
	var convoStr strings.Builder
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

	// Feed in recent reflections so the LLM knows what it already said
	// and can avoid rehashing the same observations.
	prevReflections, _ := store.RecentReflections(3)
	var prevReflStr strings.Builder
	if len(prevReflections) == 0 {
		prevReflStr.WriteString("(no previous reflections)")
	} else {
		for _, r := range prevReflections {
			fmt.Fprintf(&prevReflStr, "- [%s] %s\n", r.Timestamp.Format("Jan 2"), r.Content)
		}
	}

	prompt := fmt.Sprintf(nightlyReflectPromptTmpl,
		botName,
		currentPersona,
		traitStr.String(),
		convoStr.String(),
		userFactStr.String(),
		prevReflStr.String(),
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
		store.SetLastReflectionAt(time.Now())
		return nil
	}

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
	embedClient *embed.Client,
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

	// Get all unconsumed reflections.
	state, _ := store.GetPersonaState()
	reflections, err := store.ReflectionsSince(state.LastRewriteAt)
	if err != nil {
		return false, fmt.Errorf("loading reflections: %w", err)
	}

	newPersona, bullets, changed, err := twoStepRewrite(llmClient, embedClient, store, reflections, botName)
	if err != nil {
		return false, err
	}
	if !changed {
		log.Info("dream rewrite: analysis returned UNCHANGED, persona stable")
		store.SetLastRewriteAt(time.Now())
		return false, nil
	}

	// Stash the analysis bullets so callers (like the sim) can surface them.
	lastAnalysisBullets = bullets

	ok, err := commitPersona(llmClient, store, personaFile, newPersona, botName, fmt.Sprintf("dream: %d reflections", len(reflections)))
	if err != nil {
		return false, err
	}

	if err := store.SetLastRewriteAt(time.Now()); err != nil {
		log.Warn("failed to update last_rewrite_at", "err", err)
	}

	return ok, nil
}

// lastAnalysisBullets holds the Step 1 analysis output from the most recent
// twoStepRewrite call. Exposed via LastAnalysisBullets() for the sim reporter.
var lastAnalysisBullets string

// LastAnalysisBullets returns the analysis bullets from the most recent persona
// rewrite and clears the buffer. Used by the sim to include the two-step
// intermediate state in reports.
func LastAnalysisBullets() string {
	b := lastAnalysisBullets
	lastAnalysisBullets = ""
	return b
}

// twoStepRewrite is the core persona evolution pipeline:
//   - Step 1 (analysis): LLM distills reflections + traits + self-facts into
//     3-5 bullet points of "what is true about me right now"
//   - Step 2 (compose): LLM writes the persona from those bullets + trait scores,
//     never seeing the old persona text
//
// Returns (newPersona, analysisBullets, true, nil) on success,
// ("", "", false, nil) if UNCHANGED.
func twoStepRewrite(
	llmClient *llm.Client,
	embedClient *embed.Client,
	store memory.Store,
	reflections []memory.Reflection,
	botName string,
) (string, string, bool, error) {
	// Deduplicate reflections via embedding similarity.
	deduped := deduplicateReflections(reflections, embedClient)

	var reflStr strings.Builder
	if len(deduped) == 0 {
		reflStr.WriteString("(no reflections yet)\n")
	} else {
		for _, r := range deduped {
			fmt.Fprintf(&reflStr, "- [%s] %s\n", r.Timestamp.Format("Jan 2"), r.Content)
		}
	}

	// Load trait scores as structural guardrails.
	traits, _ := store.GetCurrentTraits()
	traitMap := make(map[string]string)
	var traitStr strings.Builder
	if len(traits) == 0 {
		traitStr.WriteString("(no trait scores yet)")
	} else {
		for _, t := range traits {
			traitMap[t.TraitName] = t.Value
			fmt.Fprintf(&traitStr, "- %s: %s\n", t.TraitName, t.Value)
		}
	}

	// Self-facts for additional context.
	selfFacts, _ := store.RecentMemories("self", 20)
	var selfStr strings.Builder
	for _, f := range selfFacts {
		fmt.Fprintf(&selfStr, "- %s\n", f.Content)
	}
	if selfStr.Len() == 0 {
		selfStr.WriteString("(no self-observations yet)\n")
	}

	// --- Step 1: Analysis ---
	analysisPrompt := fmt.Sprintf(analysisPromptTmpl, botName, traitStr.String(), reflStr.String(), selfStr.String())

	analysisResp, err := llmClient.ChatCompletion([]llm.ChatMessage{
		{Role: "system", Content: analysisPrompt},
		{Role: "user", Content: "Distill your current truths now."},
	})
	if err != nil {
		return "", "", false, fmt.Errorf("persona analysis LLM call: %w", err)
	}
	store.SaveMetric(analysisResp.Model, analysisResp.PromptTokens, analysisResp.CompletionTokens, analysisResp.TotalTokens, analysisResp.CostUSD, 0, 0, analysisResp.UsedFallback)

	bullets := strings.TrimSpace(analysisResp.Content)
	if bullets == "UNCHANGED" {
		return "", "", false, nil
	}

	log.Info("persona analysis complete", "bullets", truncate(bullets, 300))

	// --- Step 2: Compose ---
	// Look up individual trait values for the compose prompt template.
	getTraitVal := func(name, fallback string) string {
		if v, ok := traitMap[name]; ok {
			return v
		}
		return fallback
	}

	composePrompt := fmt.Sprintf(composePromptTmpl,
		botName,
		bullets,
		getTraitVal("warmth", "?"),
		getTraitVal("directness", "?"),
		getTraitVal("humor_style", "?"),
		getTraitVal("initiative", "?"),
		getTraitVal("depth", "?"),
	)

	composeResp, err := llmClient.ChatCompletion([]llm.ChatMessage{
		{Role: "system", Content: composePrompt},
		{Role: "user", Content: "Write your personality description now."},
	})
	if err != nil {
		return "", bullets, false, fmt.Errorf("persona compose LLM call: %w", err)
	}
	store.SaveMetric(composeResp.Model, composeResp.PromptTokens, composeResp.CompletionTokens, composeResp.TotalTokens, composeResp.CostUSD, 0, 0, composeResp.UsedFallback)

	newPersona := strings.TrimSpace(composeResp.Content)
	if newPersona == "" {
		return "", bullets, false, fmt.Errorf("persona compose returned empty content")
	}

	log.Info("persona compose complete", "chars", len(newPersona), "preview", truncate(newPersona, 200))
	return newPersona, bullets, true, nil
}

// deduplicateReflections filters out near-duplicate reflections using embedding
// similarity. Iterates chronologically — for each reflection, if it's too similar
// (cosine ≥ ReflectionDedupThreshold) to any already-accepted reflection, it's
// skipped. Returns the deduplicated slice.
//
// Nil-safe: if embedClient is nil, returns the input unchanged.
func deduplicateReflections(reflections []memory.Reflection, embedClient *embed.Client) []memory.Reflection {
	if embedClient == nil || len(reflections) <= 1 {
		return reflections
	}

	type accepted struct {
		reflection memory.Reflection
		vec        []float32
	}

	var kept []accepted
	for _, r := range reflections {
		vec, err := embedClient.Embed(r.Content)
		if err != nil {
			// Embedding failed — keep the reflection rather than silently dropping it.
			kept = append(kept, accepted{reflection: r})
			continue
		}

		isDupe := false
		for _, a := range kept {
			if a.vec == nil {
				continue
			}
			sim := embed.CosineSimilarity(vec, a.vec)
			if sim >= embed.ReflectionDedupThreshold {
				isDupe = true
				break
			}
		}

		if !isDupe {
			kept = append(kept, accepted{reflection: r, vec: vec})
		}
	}

	result := make([]memory.Reflection, len(kept))
	for i, a := range kept {
		result[i] = a.reflection
	}

	if len(reflections) != len(result) {
		log.Info("reflections deduplicated", "before", len(reflections), "after", len(result))
	}
	return result
}

// truncate shortens a string for log output.
func truncate(s string, maxLen int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// Package tools — shared helper for save_fact and save_self_fact handlers.
//
// Both tools call the same underlying logic with different "subject" values:
// save_fact passes "user", save_self_fact passes "self". This file holds
// the shared ExecSaveFact function so the two thin wrapper handlers don't
// duplicate logic.
//
// The blocklists and constants are also here — they used to live in agent.go
// but they belong with the logic that uses them. In Go, package-level vars
// declared in any file in a package are visible to all files in that package.
package tools

import (
	"encoding/json"
	"fmt"
	"strings"

	"her/embed"
	"her/logger"
)

// factLog is a logger for fact-saving operations.
var factLog = logger.WithPrefix("tools/fact")

// selfFactBlocklist contains phrases that indicate the agent is just
// restating its system prompt capabilities rather than saving a genuine
// learned observation. These get rejected before hitting the database.
var selfFactBlocklist = []string{
	"i can recall",
	"i am able to",
	"i have the ability",
	"my role is",
	"i am an ai",
	// Note: "i am <name>" and "my name is <name>" are checked dynamically
	// using cfg.Identity.Her — see ExecSaveFact.
	"i should be",
	"i try to be",
	"i am designed to",
	"i was created to",
	"my purpose is",
	"i am here to",
	"i can remember",
	"i can help",
}

// styleBlocklist catches AI writing tics that poison the voice over time.
// Facts with these patterns get rejected so they don't leak into the
// system prompt and infect the conversational model's tone.
var styleBlocklist = []string{
	// Em dashes — the #1 offender
	"\u2014", // —
	"\u2013", // –

	// "Not just X, it's Y" and variants
	"not just",
	"it's not just",
	"not merely",

	// Grandiose/hollow language
	"significant moment",
	"significant trust",
	"deeply personal",
	"genuinely incredible",
	"a testament to",
	"speaks volumes",

	// Corporate AI speak
	"actively investing",
	"building a bridge",
	"creating a richer",
	"meta-level",
	"hold space",
	"holding space",

	// Hollow filler
	"it's worth noting",
	"it's important to",
	"fundamentally",
	"remarkably",
	"transformative",
	"delve",
	"foster",
	"leverage",
	"tapestry",
	"realm",
	"landscape",
	"embark",
	"harness",
	"utilize",
}

// maxFactLength is the hard limit on fact text length. Facts are supposed
// to be 1-2 sentences. Multi-paragraph reflections belong in the
// persona evolution system, not in individual facts.
const maxFactLength = 200

// sameDayContextThreshold is a tighter duplicate threshold for "context"
// category facts. Multiple snapshots of the same day ("at Bolivar feeling
// low", "at Bolivar doing grounding exercise") are situational duplicates
// that the normal tag-based threshold misses. 0.70 catches these while
// still allowing genuinely different contexts on the same day.
const sameDayContextThreshold = 0.70

// StyleBlocklist returns the style blocklist so tools/update_fact can apply
// the same gates without importing agent (which would create a circular import).
func StyleBlocklist() []string {
	return styleBlocklist
}

// MaxFactLength returns the maximum allowed fact character count.
func MaxFactLength() int {
	return maxFactLength
}

// ExecSaveFact is the shared implementation behind save_fact and save_self_fact.
//
// The subject parameter distinguishes the two tools: "user" for save_fact,
// "self" for save_self_fact. Everything else is identical — same quality
// gates, same embedding strategy, same classifier check.
func ExecSaveFact(argsJSON, subject string, ctx *Context) string {
	var args struct {
		Fact       string `json:"fact"`
		Category   string `json:"category"`
		Importance int    `json:"importance"`
		Tags       string `json:"tags"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	// Clamp importance to [1, 10].
	if args.Importance < 1 {
		args.Importance = 1
	}
	if args.Importance > 10 {
		args.Importance = 10
	}

	// Quality gate for self-facts: block system-prompt restatements.
	if subject == "self" {
		lower := strings.ToLower(args.Fact)
		for _, blocked := range selfFactBlocklist {
			if strings.Contains(lower, blocked) {
				factLog.Warn("blocked self-fact (matches blocklist)", "blocklist_entry", blocked, "fact", args.Fact)
				return "rejected: this is a system capability, not a learned observation. Self-facts should only capture things learned through interaction."
			}
		}
		// Dynamic name check — "i am <name>" and "my name is <name>"
		// are identity restatements from the system prompt, not learned facts.
		nameLower := strings.ToLower(ctx.Cfg.Identity.Her)
		if strings.Contains(lower, "i am "+nameLower) || strings.Contains(lower, "my name is "+nameLower) {
			factLog.Warn("blocked self-fact (identity restatement)", "fact", args.Fact)
			return "rejected: this is an identity restatement from the system prompt, not a learned observation."
		}
	}

	// Style gate for ALL facts: reject AI writing tics.
	lower := strings.ToLower(args.Fact)
	for _, blocked := range styleBlocklist {
		if strings.Contains(lower, blocked) {
			factLog.Warn("blocked fact (style)", "pattern", blocked, "fact", args.Fact)
			return fmt.Sprintf("rejected: rewrite this fact in plain, concise language. Avoid em dashes, 'not just X it's Y', and grandiose phrasing. Keep it under 2 sentences. The blocked pattern was: %q", blocked)
		}
	}

	// Length gate: facts should be 1-2 sentences, not paragraphs.
	if len(args.Fact) > maxFactLength {
		factLog.Warn("blocked fact (too long)", "len", len(args.Fact), "fact", args.Fact[:100])
		return fmt.Sprintf("rejected: fact is %d characters (max %d). Condense to 1-2 short sentences.", len(args.Fact), maxFactLength)
	}

	// Strip temporal references (dates, "today", "last Tuesday", etc.) before
	// embedding or saving. The DB timestamp handles "when" — fact text should
	// be timeless so it stays accurate as time passes.
	args.Fact = StripTimestamps(args.Fact)

	// Embed by TAGS (not by fact text) so the vector space organizes by
	// topic. "mental health, burnout, coping" lands far from "programming,
	// go, backend" — which is what we want for retrieval. Fall back to
	// fact text if the agent didn't provide tags.
	embedText := args.Tags
	if embedText == "" {
		embedText = args.Fact
	}

	var newVec []float32
	var textVec []float32
	if ctx.EmbedClient != nil {
		var err error
		newVec, err = ctx.EmbedClient.Embed(embedText)
		if err != nil {
			factLog.Warn("embedding failed, skipping duplicate check", "err", err)
		} else {
			// Also embed the raw fact text for a second similarity check.
			// Tags catch topical duplicates but miss situational duplicates
			// where the same event is described from different tag angles.
			if args.Tags != "" {
				textVec, err = ctx.EmbedClient.Embed(args.Fact)
				if err != nil {
					factLog.Warn("text embedding failed, using tag-only dedup", "err", err)
				}
			}

			// Same-day context facts use a tighter threshold.
			threshold := ctx.SimilarityThreshold
			if args.Category == "context" {
				threshold = sameDayContextThreshold
			}

			if duplicate, existingID, existingFact, sim, source := checkFactDuplicate(newVec, textVec, subject, threshold, ctx); duplicate {
				factLog.Info("blocked duplicate fact", "similarity_pct", sim*100, "existing_id", existingID, "source", source, "fact", args.Fact)
				return fmt.Sprintf("rejected: too similar (%.0f%%) to existing fact ID=%d (%q) [matched on %s]. Use update_fact to refine it instead.",
					sim*100, existingID, existingFact, source)
			}
		}
	}

	// --- Classifier gate ---
	// Runs AFTER style/length/dedup gates — no point classifying something
	// that would be rejected anyway. Fail-open if classifier is nil or
	// if the agent didn't inject a classify function.
	if ctx.ClassifierLLM != nil && ctx.ClassifyWriteFunc != nil {
		writeType := "fact"
		if subject == "self" {
			writeType = "self_fact"
		}
		snippet, _ := ctx.Store.RecentMessages(ctx.ConversationID, 3)
		verdict := ctx.ClassifyWriteFunc(writeType, args.Fact, snippet)
		if !verdict.Allowed {
			if ctx.RejectionMessageFunc != nil {
				return ctx.RejectionMessageFunc(verdict)
			}
			return fmt.Sprintf("rejected by classifier: %s", verdict.Reason)
		}
	}

	id, err := ctx.Store.SaveFact(args.Fact, args.Category, subject, 0, args.Importance, newVec, textVec, args.Tags)
	if err != nil {
		return fmt.Sprintf("error saving fact: %v", err)
	}

	label := "user fact"
	if subject == "self" {
		label = "self fact"
	}

	ctx.SavedFacts = append(ctx.SavedFacts, args.Fact)

	return fmt.Sprintf("saved %s ID=%d: %s", label, id, args.Fact)
}

// checkFactDuplicate compares a new fact against all existing facts using two
// embedding strategies: tag-based (topical) and text-based (semantic).
// If either similarity exceeds the threshold, the fact is a duplicate.
//
// The returned "source" string indicates which check caught the duplicate
// ("tags" or "text") for logging/debugging.
func checkFactDuplicate(newTagVec, newTextVec []float32, subject string, threshold float64, ctx *Context) (isDuplicate bool, existingID int64, existingFact string, similarity float64, source string) {
	existingFacts, err := ctx.Store.AllActiveFacts()
	if err != nil {
		factLog.Warn("couldn't load facts for duplicate check", "err", err)
		return false, 0, "", 0, ""
	}

	var bestSim float64
	var bestID int64
	var bestFact string
	var bestSource string

	for _, existing := range existingFacts {
		if existing.Subject != subject {
			continue
		}

		// --- Tag-based similarity (topical dedup) ---
		existTagVec := existing.Embedding
		if len(existTagVec) == 0 {
			embedText := existing.Tags
			if embedText == "" {
				embedText = existing.Fact
			}
			existTagVec, err = ctx.EmbedClient.Embed(embedText)
			if err != nil {
				continue
			}
			// Backfill: persist the computed tag embedding.
			_ = ctx.Store.UpdateFactEmbedding(existing.ID, existTagVec, existing.EmbeddingText)
			factLog.Debug("backfilled tag embedding for fact", "fact_id", existing.ID)
		}

		tagSim := embed.CosineSimilarity(newTagVec, existTagVec)
		if tagSim > bestSim {
			bestSim = tagSim
			bestID = existing.ID
			bestFact = existing.Fact
			bestSource = "tags"
		}

		// --- Text-based similarity (semantic dedup) ---
		if len(newTextVec) > 0 {
			existTextVec := existing.EmbeddingText
			if len(existTextVec) == 0 {
				existTextVec, err = ctx.EmbedClient.Embed(existing.Fact)
				if err != nil {
					continue
				}
				_ = ctx.Store.UpdateFactEmbedding(existing.ID, existing.Embedding, existTextVec)
				factLog.Debug("backfilled text embedding for fact", "fact_id", existing.ID)
			}
			textSim := embed.CosineSimilarity(newTextVec, existTextVec)
			if textSim > bestSim {
				bestSim = textSim
				bestID = existing.ID
				bestFact = existing.Fact
				bestSource = "text"
			}
		}
	}

	if bestSim >= threshold {
		return true, bestID, bestFact, bestSim, bestSource
	}
	return false, 0, "", 0, ""
}

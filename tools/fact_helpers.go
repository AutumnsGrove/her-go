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

	"her/classifier"
	"her/embed"
	"her/logger"
)

// factLog is a logger for fact-saving operations.
var factLog = logger.WithPrefix("tools/fact")

// styleBlocklist catches AI writing tics that poison the voice over time.
// Facts with these patterns get rejected so they don't leak into the
// system prompt and infect the conversational model's tone.
//
// Note: em dashes mid-sentence are fine (normal prose). The tic we're
// catching is a trailing em dash — where a sentence just hangs off "—"
// at the end with nothing after it. That's checked separately in
// ExecSaveFact using a suffix check, not a Contains check.
//
// "hold space" / "holding space" are intentionally absent — the bot uses
// these phrases genuinely in self_facts and the classifier handles quality.
var styleBlocklist = []string{
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
	"embark",
	"harness",
	"utilize",
}

// maxFactLength is the hard limit on fact text length. Facts are supposed
// to be 1-2 sentences. Multi-paragraph reflections belong in the
// persona evolution system, not in individual facts.
const maxFactLength = 250

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
		Fact     string `json:"fact"`
		Category string `json:"category"`
		Tags     string `json:"tags"`
		Context  string `json:"context"` // optional: why this fact matters
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	// Style gate for ALL facts: reject AI writing tics.
	// Em dashes mid-sentence are fine (normal prose punctuation). The tic
	// we catch is a TRAILING em dash — a sentence that hangs with "—" at
	// the end and nothing after it. That's the specific hallmark of AI slop.
	trimmed := strings.TrimSpace(args.Fact)
	if strings.HasSuffix(trimmed, "\u2014") || strings.HasSuffix(trimmed, "\u2013") {
		factLog.Warn("blocked fact (trailing em dash)", "fact", args.Fact)
		return "rejected: rewrite this fact — it ends with a trailing em dash. Complete the sentence."
	}
	lower := strings.ToLower(args.Fact)
	for _, blocked := range styleBlocklist {
		if strings.Contains(lower, blocked) {
			factLog.Warn("blocked fact (style)", "pattern", blocked, "fact", args.Fact)
			return fmt.Sprintf("rejected: rewrite this fact in plain, concise language. Avoid 'not just X it's Y' and grandiose phrasing. Keep it under 2 sentences. The blocked pattern was: %q", blocked)
		}
	}

	// Length gate: facts should be 1-2 sentences, not paragraphs.
	if len(args.Fact) > maxFactLength {
		factLog.Warn("blocked fact (too long)", "len", len(args.Fact), "fact", args.Fact[:100])
		return fmt.Sprintf("rejected: fact is %d characters (max %d). Condense to 1-2 short sentences.", len(args.Fact), maxFactLength)
	}

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
			// When context is provided, include it in the text embedding so
			// semantic search is aware of the "why", not just the "what".
			if args.Tags != "" {
				factTextForEmbed := args.Fact
				if args.Context != "" {
					factTextForEmbed = args.Fact + " " + args.Context
				}
				textVec, err = ctx.EmbedClient.Embed(factTextForEmbed)
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
	// that would be rejected anyway. Fail-open if classifier is nil.
	//
	// Pre-approved bypass: if the classifier previously suggested this exact
	// text as a rewrite, skip re-classification. This prevents the self-
	// contradiction bug where the classifier rejects its own suggestion.
	if ctx.ClassifierLLM != nil {
		if ctx.PreApprovedRewrites != nil && ctx.PreApprovedRewrites[strings.ToLower(args.Fact)] {
			log.Info("classifier bypass: fact matches pre-approved rewrite", "fact", args.Fact)
		} else {
			writeType := "fact"
			if subject == "self" {
				writeType = "self_fact"
			}
			// Use pre-captured snippet when available (memory agent sets this to
			// avoid the timing bug where later turns pollute the DB before the
			// goroutine reaches the classifier). Fall back to lazy query otherwise.
			snippet := ctx.ClassifierSnippet
			if snippet == nil {
				snippet, _ = ctx.Store.RecentMessages(ctx.ConversationID, 1)
			}
			verdict := classifier.Check(ctx.ClassifierLLM, writeType, args.Fact, snippet)
			_ = ctx.Store.SaveClassifierLog(
				ctx.ConversationID, writeType, verdict.Type, args.Fact, verdict.Reason, verdict.Rewrite,
			)
			if verdict.Rewrite != "" && ctx.PreApprovedRewrites != nil {
				ctx.PreApprovedRewrites[strings.ToLower(verdict.Rewrite)] = true
			}
			if !verdict.Allowed {
				return classifier.RejectionMessage(verdict)
			}
		}
	}

	if ctx.Store == nil {
		return "error: no store configured"
	}

	id, err := ctx.Store.SaveFact(args.Fact, args.Category, subject, 0, 5, newVec, textVec, args.Tags, args.Context)
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


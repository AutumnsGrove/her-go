// Package memgate consolidates the memory write quality pipeline that was
// previously duplicated across save_memory, update_memory, and merge_memories.
//
// Every memory write passes through the same ordered gate sequence:
//
//  1. Style blocklist  — catches AI writing tics  (~0ms, string match)
//  2. Length gate       — enforces max char limit  (~0ms, len() check)
//  3. Embedding dedup   — two-vector similarity    (~50ms, skippable)
//  4. Classifier LLM    — quality/factuality gate  (~200-500ms)
//  5. Safety classifier — self-memory sycophancy   (~200-500ms, self only)
//
// Gates are ordered cheapest-first. The pipeline returns early on the first
// rejection so expensive gates never run when a cheap gate would have caught it.
//
// Fail-open design: if the classifier LLM is nil or returns an error,
// the pipeline allows the write rather than blocking it.
package memgate

import (
	"fmt"
	"strings"

	"her/classifier"
	"her/embed"
	"her/llm"
	"her/logger"
	"her/memory"
)

var log = logger.WithPrefix("memgate")

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// Verdict is the result of running the write pipeline.
type Verdict struct {
	// Allowed is true if the memory passed all gates.
	Allowed bool

	// Reason is a human-readable rejection message (empty if allowed).
	Reason string

	// Rewrite is a classifier-suggested alternative text (empty if none).
	Rewrite string

	// Splits contains sub-memories from a SPLIT verdict (empty if not split).
	Splits []string

	// IsSplit is true when the classifier returned a SPLIT verdict with
	// 2+ sub-memories. The caller should save the splits instead of the
	// original text.
	IsSplit bool

	// DuplicateID is the ID of the existing memory that matched during
	// dedup (0 if no duplicate found or dedup was skipped).
	DuplicateID int64

	// TagVec is the tag embedding computed during dedup. Callers can
	// reuse it for SaveMemory instead of re-embedding. nil if dedup
	// was skipped or embedding failed.
	TagVec []float32

	// TextVec is the text embedding computed during dedup. nil if no
	// separate text vector was needed (no tags provided) or dedup skipped.
	TextVec []float32
}

// PipelineInput describes the memory text being validated.
type PipelineInput struct {
	// Text is the memory content to validate.
	Text string

	// Subject is "user" or "self".
	Subject string

	// Tags are the memory's topic tags (used for embedding).
	Tags string

	// Category is the memory category (e.g. "context").
	Category string

	// Context is optional "why this matters" text.
	Context string

	// OldText is non-empty for updates — shows the classifier the delta
	// by formatting as "Original memory: X\nUpdated memory: Y".
	OldText string
}

// PipelineDeps bundles the external dependencies the pipeline needs.
// All fields except MaxLength are nil-safe.
type PipelineDeps struct {
	// Store is used for classifier logging and metric saving.
	Store memory.Store

	// EmbedClient computes embeddings for dedup. nil = dedup skipped.
	EmbedClient *embed.Client

	// ClassifierLLM runs the quality gate. nil = classifier skipped (fail-open).
	ClassifierLLM *llm.Client

	// SimilarityThreshold is the cosine similarity threshold for dedup.
	SimilarityThreshold float64

	// MaxLength is the hard character limit. 0 = use default (1000).
	MaxLength int

	// ConversationID links classifier logs to the conversation.
	ConversationID string

	// TriggerMsgID links metrics to the originating message.
	TriggerMsgID int64

	// Snippet is pre-captured conversation context for the classifier.
	// nil = the pipeline will lazy-query from the store.
	Snippet []memory.Message

	// PreApproved is a map of classifier-suggested rewrites that bypass
	// re-classification. The pipeline reads and writes this map.
	// nil = no pre-approved bypass.
	PreApproved map[string]bool

	// SkipDedup skips the embedding duplicate check. Used by merge_memories
	// (sources are already deactivated) and update_memory.
	SkipDedup bool
}

// ---------------------------------------------------------------------------
// Style blocklist
// ---------------------------------------------------------------------------

// styleBlocklist catches AI writing tics that poison the voice over time.
var styleBlocklist = []string{
	"not just",
	"it's not just",
	"not merely",
	"significant moment",
	"significant trust",
	"deeply personal",
	"genuinely incredible",
	"a testament to",
	"speaks volumes",
	"actively investing",
	"building a bridge",
	"creating a richer",
	"meta-level",
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

const defaultMaxLength = 1000

// StyleBlocklist returns the blocklist for external callers that need it
// (e.g. update_memory's rejection messages).
func StyleBlocklist() []string {
	return styleBlocklist
}

// MaxLength returns the default max memory length.
func MaxLength() int {
	return defaultMaxLength
}

// ---------------------------------------------------------------------------
// Pipeline
// ---------------------------------------------------------------------------

// RunPipeline validates a memory write through all gates in order.
// Returns early on the first rejection.
func RunPipeline(input PipelineInput, deps PipelineDeps) Verdict {
	// -- Gate 1: Style blocklist --
	trimmed := strings.TrimSpace(input.Text)
	if strings.HasSuffix(trimmed, "—") || strings.HasSuffix(trimmed, "–") {
		log.Warn("blocked (trailing em dash)", "memory", input.Text)
		return Verdict{
			Reason: "rejected: rewrite this memory — it ends with a trailing em dash. Complete the sentence.",
		}
	}
	lower := strings.ToLower(input.Text)
	for _, blocked := range styleBlocklist {
		if strings.Contains(lower, blocked) {
			log.Warn("blocked (style)", "pattern", blocked, "memory", input.Text)
			return Verdict{
				Reason: fmt.Sprintf("rejected: rewrite this memory in plain, concise language. Avoid 'not just X it's Y' and grandiose phrasing. Keep it under 2 sentences. The blocked pattern was: %q", blocked),
			}
		}
	}

	// -- Gate 2: Length --
	limit := deps.MaxLength
	if limit <= 0 {
		limit = defaultMaxLength
	}
	if len(input.Text) > limit {
		log.Warn("blocked (too long)", "len", len(input.Text), "memory", input.Text[:min(len(input.Text), 100)])
		return Verdict{
			Reason: fmt.Sprintf("rejected: memory is %d characters (max %d). Condense to 1-2 short sentences.", len(input.Text), limit),
		}
	}

	// -- Gate 3: Embedding dedup --
	var tagVec, textVec []float32
	if !deps.SkipDedup && deps.EmbedClient != nil {
		v := runDedupGate(input, deps)
		tagVec = v.TagVec
		textVec = v.TextVec
		if !v.Allowed {
			return v
		}
	}

	// -- Gate 4: Classifier LLM --
	if deps.ClassifierLLM != nil {
		if v := runClassifierGate(input, deps); !v.Allowed || v.IsSplit {
			return v
		}
	}

	return Verdict{Allowed: true, TagVec: tagVec, TextVec: textVec}
}

// ---------------------------------------------------------------------------
// Gate 3: Dedup
// ---------------------------------------------------------------------------

func runDedupGate(input PipelineInput, deps PipelineDeps) Verdict {
	embedText := input.Tags
	if embedText == "" {
		embedText = input.Text
	}

	newTagVec, err := deps.EmbedClient.Embed(embedText)
	if err != nil {
		log.Warn("embedding failed, skipping duplicate check", "err", err)
		return Verdict{Allowed: true}
	}

	var newTextVec []float32
	if input.Tags != "" {
		memTextForEmbed := input.Text
		if input.Context != "" {
			memTextForEmbed = input.Text + " " + input.Context
		}
		newTextVec, err = deps.EmbedClient.Embed(memTextForEmbed)
		if err != nil {
			log.Warn("text embedding failed, using tag-only dedup", "err", err)
		}
	}

	threshold := deps.SimilarityThreshold
	if input.Category == "context" {
		threshold = embed.ContextMemorySimilarityThreshold
	}

	isDup, existingID, existingContent, sim, source := checkDuplicate(
		newTagVec, newTextVec, input.Subject, threshold, deps,
	)
	if isDup {
		log.Info("blocked duplicate memory",
			"similarity_pct", sim*100,
			"existing_id", existingID,
			"source", source,
			"memory", input.Text)
		return Verdict{
			Reason: fmt.Sprintf("rejected: too similar (%.0f%%) to existing memory ID=%d (%q) [matched on %s]. Use update_memory to refine it instead.",
				sim*100, existingID, existingContent, source),
			DuplicateID: existingID,
			TagVec:      newTagVec,
			TextVec:     newTextVec,
		}
	}

	return Verdict{Allowed: true, TagVec: newTagVec, TextVec: newTextVec}
}

// checkDuplicate compares a new memory against all existing memories using
// two embedding strategies: tag-based (topical) and text-based (semantic).
func checkDuplicate(newTagVec, newTextVec []float32, subject string, threshold float64, deps PipelineDeps) (isDuplicate bool, existingID int64, existingContent string, similarity float64, source string) {
	existingMemories, err := deps.Store.AllActiveMemories()
	if err != nil {
		log.Warn("couldn't load memories for duplicate check", "err", err)
		return false, 0, "", 0, ""
	}

	tagCandidates := make(map[int64][]float32)
	textCandidates := make(map[int64][]float32)

	for _, existing := range existingMemories {
		if existing.Subject != subject {
			continue
		}

		existTagVec := existing.Embedding
		if len(existTagVec) == 0 {
			embedText := existing.Tags
			if embedText == "" {
				embedText = existing.Content
			}
			existTagVec, err = deps.EmbedClient.Embed(embedText)
			if err != nil {
				continue
			}
			_ = deps.Store.UpdateMemoryEmbedding(existing.ID, existTagVec, existing.EmbeddingText)
			log.Debug("backfilled tag embedding for memory", "memory_id", existing.ID)
		}
		tagCandidates[existing.ID] = existTagVec

		if len(newTextVec) > 0 {
			existTextVec := existing.EmbeddingText
			if len(existTextVec) == 0 {
				existTextVec, err = deps.EmbedClient.Embed(existing.Content)
				if err != nil {
					continue
				}
				_ = deps.Store.UpdateMemoryEmbedding(existing.ID, existing.Embedding, existTextVec)
				log.Debug("backfilled text embedding for memory", "memory_id", existing.ID)
			}
			textCandidates[existing.ID] = existTextVec
		}
	}

	tagID, tagSim, _ := embed.FindBestMatch(newTagVec, tagCandidates, 0, false)
	textID, textSim, _ := embed.FindBestMatch(newTextVec, textCandidates, 0, false)

	var bestID int64
	var bestSim float64
	var bestSource string

	if textSim > tagSim {
		bestID = textID
		bestSim = textSim
		bestSource = "text"
	} else if tagSim > 0 {
		bestID = tagID
		bestSim = tagSim
		bestSource = "tags"
	}

	if bestSim < threshold || bestID == 0 {
		return false, 0, "", 0, ""
	}

	content, _ := deps.Store.GetMemoryContent(bestID)
	return true, bestID, content, bestSim, bestSource
}

// ---------------------------------------------------------------------------
// Gate 4+5: Classifier + Safety
// ---------------------------------------------------------------------------

func runClassifierGate(input PipelineInput, deps PipelineDeps) Verdict {
	// Pre-approved bypass: if the classifier previously suggested this exact
	// text as a rewrite, skip re-classification.
	if deps.PreApproved != nil && deps.PreApproved[strings.ToLower(input.Text)] {
		log.Info("classifier bypass: memory matches pre-approved rewrite", "memory", input.Text)
		return Verdict{Allowed: true}
	}

	writeType := "memory"
	if input.Subject == "self" {
		writeType = "self_memory"
	}

	// For updates, show the classifier both old and new text.
	classifyContent := input.Text
	if input.OldText != "" {
		classifyContent = fmt.Sprintf("Original memory: %s\nUpdated memory: %s", input.OldText, input.Text)
	}

	// Use pre-captured snippet or lazy-query.
	snippet := deps.Snippet
	if snippet == nil && deps.Store != nil {
		snippet, _ = deps.Store.RecentMessages(deps.ConversationID, 1)
	}

	verdict := classifier.Check(deps.ClassifierLLM, writeType, classifyContent, snippet)

	// Save classifier metrics.
	if verdict.Model != "" && deps.Store != nil {
		_ = deps.Store.SaveMetric(memory.MetricInput{
			Model: verdict.Model, PromptTokens: verdict.PromptTokens,
			CompletionTokens: verdict.CompletionTokens, TotalTokens: verdict.TotalTokens,
			CostUSD: verdict.CostUSD, MessageID: deps.TriggerMsgID,
			AgentRole: memory.RoleClassifier, CacheReadTokens: verdict.CacheReadTokens,
			CacheWriteTokens: verdict.CacheWriteTokens, Provider: verdict.Provider,
		})
	}

	// Save classifier log.
	if deps.Store != nil {
		_ = deps.Store.SaveClassifierLog(
			deps.ConversationID, writeType, verdict.Type,
			classifyContent, verdict.Reason, verdict.Rewrite,
		)
	}

	// Track pre-approved rewrites.
	if verdict.Rewrite != "" && deps.PreApproved != nil {
		deps.PreApproved[strings.ToLower(verdict.Rewrite)] = true
	}

	// SPLIT: classifier says this memory packs multiple distinct ideas.
	if verdict.Type == "SPLIT" && len(verdict.Splits) >= 2 {
		log.Info("classifier: SPLIT verdict", "count", len(verdict.Splits), "reason", verdict.Reason)
		return Verdict{
			Allowed: true,
			IsSplit: true,
			Splits:  verdict.Splits,
			Rewrite: verdict.Rewrite,
		}
	}

	if !verdict.Allowed {
		return Verdict{
			Reason:  classifier.RejectionMessage(verdict),
			Rewrite: verdict.Rewrite,
		}
	}

	// -- Gate 5: Self-memory safety --
	// Separate focused check that catches feedback-loop patterns.
	// Only runs for self-memories. Independent LLM call.
	if input.Subject == "self" {
		safetyVerdict := classifier.Check(deps.ClassifierLLM, "self_memory_safety", input.Text, snippet)
		if safetyVerdict.Model != "" && deps.Store != nil {
			_ = deps.Store.SaveMetric(memory.MetricInput{
				Model: safetyVerdict.Model, PromptTokens: safetyVerdict.PromptTokens,
				CompletionTokens: safetyVerdict.CompletionTokens, TotalTokens: safetyVerdict.TotalTokens,
				CostUSD: safetyVerdict.CostUSD, MessageID: deps.TriggerMsgID,
				AgentRole: memory.RoleClassifier, CacheReadTokens: safetyVerdict.CacheReadTokens,
				CacheWriteTokens: safetyVerdict.CacheWriteTokens, Provider: safetyVerdict.Provider,
			})
		}
		if deps.Store != nil {
			_ = deps.Store.SaveClassifierLog(
				deps.ConversationID, "self_memory_safety", safetyVerdict.Type,
				input.Text, safetyVerdict.Reason, "",
			)
		}
		if !safetyVerdict.Allowed {
			log.Warn("self-memory safety gate rejected",
				"verdict", safetyVerdict.Type, "reason", safetyVerdict.Reason)
			return Verdict{
				Reason: fmt.Sprintf("rejected: %s. Do not save self-memories that encode agreement or validation as effective strategies.",
					safetyVerdict.Reason),
			}
		}
	}

	return Verdict{Allowed: true}
}

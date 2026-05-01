package mood

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"her/embed"
	"her/llm"
	"her/logger"
	"her/memory"
	"her/trace"
	"her/turn"
)

var log = logger.WithPrefix("mood")

// Register the mood agent's trace stream. Order 300 puts it after
// main (100) and memory (200) when both are present.
func init() {
	trace.Register(trace.Stream{Name: "mood", Order: 300, Label: "🎭 <b>mood</b>"})
	turn.Register(turn.Phase{Name: "mood", Order: 300, Emoji: "🎭", Label: "mood"})
}

// Action describes what the agent did with a given turn. One action
// per RunAgent call; result.Action tells the caller exactly what
// happened so tests and the TUI can assert on it.
type Action string

const (
	ActionAutoLogged      Action = "auto_logged"
	ActionUpdated         Action = "updated_existing"
	ActionProposalEmitted Action = "proposal_emitted"
	ActionDroppedLow      Action = "dropped_low_confidence"
	ActionDroppedNoSignal Action = "dropped_no_signal"
	ActionDroppedDedup    Action = "dropped_dedup"
	ActionDroppedVocab    Action = "dropped_no_valid_labels"
	ActionDroppedClassify Action = "dropped_classifier_rejected"
	ActionErrored         Action = "errored"
)

// Inference is the structured JSON the mood LLM is asked to emit. If
// the turn has no mood signal, the model sets Skip=true with a Reason.
type Inference struct {
	Skip         bool     `json:"skip"`
	Reason       string   `json:"reason"`
	Valence      int      `json:"valence"`
	Labels       []string `json:"labels"`
	Associations []string `json:"associations"`
	Note         string   `json:"note"`
	Confidence   float64  `json:"confidence"`
	Signals      []string `json:"signals"`
}

// AgentConfig governs decision thresholds and windows. Every field
// has a sensible default — pass a zero-value AgentConfig if you want
// the defaults documented in docs/plans/PLAN-mood-tracking-redesign.md.
type AgentConfig struct {
	// ContextTurns is how many recent user+assistant turns the agent
	// sees. Default 5.
	ContextTurns int

	// ConfidenceHigh is the threshold for auto-logging without user
	// confirmation. Default 0.75.
	ConfidenceHigh float64

	// ConfidenceLow is the threshold below which inferences are
	// dropped silently (not even proposed). Default 0.40.
	ConfidenceLow float64

	// DedupWindow is how far back we look for near-duplicate mood
	// entries when deciding whether to skip the new inference.
	// Default 2 hours.
	DedupWindow time.Duration

	// DedupSimilarity is the cosine-similarity threshold that counts
	// as "same mood again". If a neighbor within DedupWindow is at
	// least this similar, we skip the write. Default 0.80.
	DedupSimilarity float64

	// UpdateSimilarity is the lower threshold for the "refine" path.
	// When a neighbor is between UpdateSimilarity and DedupSimilarity
	// (and within ±1 valence), we update the existing entry in place
	// instead of creating a duplicate. Default 0.55.
	UpdateSimilarity float64

	// UpdateMaxValenceDrift is the maximum valence difference that
	// still counts as "same mood, evolving." Beyond this the emotional
	// state has shifted enough to warrant a new entry. Default 1.
	UpdateMaxValenceDrift int

	// ProposalExpiry is how long a medium-confidence proposal stays
	// tappable before the sweeper edits it to "expired". Default 30m.
	ProposalExpiry time.Duration

	// SignalThreshold is the minimum ScoreSignals() score required to
	// proceed to the LLM call. Turns below this threshold are dropped
	// immediately without inference (cheap filter for greetings,
	// factual questions, etc.). Default 0.15 (catches bare affect words
	// like "stressed" but skips neutral turns). Set to 0 to disable.
	// When left unset (0 or negative), defaults to 0.15.
	SignalThreshold float64

	// SessionGap is the minimum time gap between messages that counts
	// as a conversation boundary. When a gap this large is found in the
	// turn history, only messages after the gap are kept — stale
	// emotional context from hours/days ago doesn't bleed into the
	// current session's mood inference. Default 4 hours.
	SessionGap time.Duration
}

// withDefaults returns a copy of c with every zero-value field filled
// in. Callers mutate the return, not the input — keeps the original
// config visibly declarative.
func (c AgentConfig) withDefaults() AgentConfig {
	if c.ContextTurns <= 0 {
		c.ContextTurns = 5
	}
	if c.ConfidenceHigh <= 0 {
		c.ConfidenceHigh = 0.75
	}
	if c.ConfidenceLow <= 0 {
		c.ConfidenceLow = 0.40
	}
	if c.DedupWindow <= 0 {
		c.DedupWindow = 2 * time.Hour
	}
	if c.DedupSimilarity <= 0 {
		c.DedupSimilarity = embed.HighSimilarityThreshold
	}
	if c.UpdateSimilarity <= 0 {
		c.UpdateSimilarity = embed.MediumSimilarityThreshold
	}
	if c.UpdateMaxValenceDrift <= 0 {
		c.UpdateMaxValenceDrift = 1
	}
	if c.ProposalExpiry <= 0 {
		c.ProposalExpiry = 30 * time.Minute
	}
	if c.SignalThreshold <= 0 {
		c.SignalThreshold = 0.15
	}
	if c.SessionGap <= 0 {
		c.SessionGap = 4 * time.Hour
	}
	return c
}

// Turn is one entry in the rolling conversation window the mood agent
// sees. Role is "user" or "assistant"; ScrubbedContent is PII-safe
// text (the agent NEVER sees raw content).
type Turn struct {
	Role            string
	ScrubbedContent string
	Timestamp       time.Time
}

// Deps are everything RunAgent needs. Designed so the sim can stub
// each field independently (LLM → FakeLLM, Embed → deterministic
// function, Propose → FakeTransport recorder).
type Deps struct {
	// LLM is the mood agent's model. Required.
	LLM *llm.Client

	// Classifier is the optional fail-open classifier model. When
	// nil, every inference passes the classifier step.
	Classifier *llm.Client

	// Store is the mood + proposal store. Required.
	Store memory.Store

	// Vocab validates labels/associations returned by the LLM.
	// Required — drop any label not in the vocab before hitting the DB.
	Vocab *Vocab

	// Embed computes the note+labels embedding. When nil, we save
	// without an embedding and dedup is skipped — that's fine for
	// tests and first-boot, it just means one duplicate might sneak
	// through.
	Embed func(ctx context.Context, text string) ([]float32, error)

	// Propose hands a medium-confidence inference to the outer world
	// (usually a Telegram inline keyboard). It returns the chat +
	// message IDs so the agent can persist the pending proposal row
	// for later callback routing. When Propose is nil, medium-
	// confidence inferences are dropped silently with reason
	// "no propose handler".
	Propose func(ctx context.Context, entry *memory.MoodEntry, expiresAt time.Time) (chatID, msgID int64, err error)

	// Clock lets sim scenarios freeze time for deterministic
	// assertions. Default: time.Now.
	Clock func() time.Time

	// ConversationID is stamped onto every inferred entry so later
	// analysis can reconstruct which conversation the mood came
	// from. Empty means "unknown" (stored as NULL).
	ConversationID string

	// PromptDir is the directory containing mood_agent_prompt.md.
	// Typically the project root (same dir as prompt.md and config.yaml).
	// Empty string falls back to the hardcoded default prompt.
	PromptDir string

	// Trace is optional — when non-nil, RunAgent calls it at each
	// decision point with a cumulative HTML trace string. Same
	// signature as the memory agent's trace callback so the bot can
	// hand the mood agent a callback that writes into a shared
	// TraceBoard slot. Leaving it nil disables tracing.
	Trace func(html string) error
}

// Result is what RunAgent reports back. Exactly one of Entry or
// Proposal is set when Action indicates a successful outcome.
type Result struct {
	Action     Action
	Reason     string
	Confidence float64
	Inference  *Inference
	Entry      *memory.MoodEntry
	Proposal   *memory.PendingMoodProposal
}

// RunAgent is the main entry point. Given the latest conversation
// window (already scrubbed), it calls the LLM, scores confidence,
// checks for dedup, and either auto-logs, emits a proposal, or drops.
//
// The caller typically runs RunAgent in a goroutine post-reply. It
// never panics (handler panics inside Propose/Embed are caught and
// surfaced via Result.Action = ActionErrored).
func RunAgent(ctx context.Context, deps Deps, cfg AgentConfig, turns []Turn) Result {
	cfg = cfg.withDefaults()
	if deps.Vocab == nil {
		return errResult(fmt.Errorf("mood.RunAgent: nil Vocab"))
	}
	if deps.Store == nil {
		return errResult(fmt.Errorf("mood.RunAgent: nil Store"))
	}
	if deps.LLM == nil {
		return errResult(fmt.Errorf("mood.RunAgent: nil LLM"))
	}
	if deps.Clock == nil {
		deps.Clock = time.Now
	}
	if len(turns) == 0 {
		return Result{Action: ActionDroppedNoSignal, Reason: "no turns"}
	}

	// Trim to the last N turns per config.
	if len(turns) > cfg.ContextTurns {
		turns = turns[len(turns)-cfg.ContextTurns:]
	}

	// Session gap filter: scan backwards for a time gap larger than
	// SessionGap. If found, discard everything before it — stale
	// emotional context from a previous session shouldn't bleed into
	// the current mood inference. Example: a heavy conversation last
	// night + "hey mira" this morning → only "hey mira" survives.
	turns = trimToCurrentSession(turns, cfg.SessionGap)
	if len(turns) == 0 {
		return Result{Action: ActionDroppedNoSignal, Reason: "no turns in current session (gap filter)"}
	}

	// Trace collector — accumulates HTML lines and flushes to the
	// callback on every meaningful step. The slot's label header
	// ("🎭 mood") is owned by the trace registry and prepended at
	// render time, so we only send body content here. Safe to call
	// even when the callback is nil.
	var tb traceBuf
	tb.emit(deps.Trace, "thinking…")

	// Pre-gate: cheap heuristic check for mood signals. If the score
	// is below the threshold, skip the LLM call entirely — saves API
	// cost on greetings, factual questions, etc. The LLM is the final
	// authority, but this filters obvious non-emotional turns.
	heuristicScore := ScoreSignals(turns)
	if heuristicScore < cfg.SignalThreshold {
		reason := fmt.Sprintf("heuristic signal score %.2f below threshold %.2f", heuristicScore, cfg.SignalThreshold)
		tb.line(fmt.Sprintf("⏭️ pre-gate: score %.2f < %.2f — skipped", heuristicScore, cfg.SignalThreshold))
		tb.flush(deps.Trace)
		return Result{
			Action: ActionDroppedNoSignal,
			Reason: reason,
		}
	}
	tb.line(fmt.Sprintf("pre-gate: score %.2f ≥ %.2f — proceeding", heuristicScore, cfg.SignalThreshold))
	tb.flush(deps.Trace)

	// Query the 3 most recent momentary moods to show the LLM what was
	// already logged. This helps it skip near-duplicates. Errors are
	// non-fatal — we just proceed without context if the query fails.
	var recentMoodLines []string
	recentEntries, err := deps.Store.RecentMoodEntries(memory.MoodKindMomentary, 3)
	if err == nil && len(recentEntries) > 0 {
		for _, e := range recentEntries {
			// Format: "#10: valence 2, [Sad, Overwhelmed, Hopeless]"
			labelStr := strings.Join(e.Labels, ", ")
			line := fmt.Sprintf("#%d: valence %d, [%s]", e.ID, e.Valence, labelStr)
			recentMoodLines = append(recentMoodLines, line)
		}
	}

	// 1. Ask the LLM.
	inference, err := callLLM(ctx, deps.LLM, deps.Vocab, turns, deps.PromptDir, recentMoodLines)
	if err != nil {
		log.Error("mood agent LLM call failed", "err", err)
		tb.line(fmt.Sprintf("⚠️ llm error: %s", err))
		tb.flush(deps.Trace)
		return Result{Action: ActionErrored, Reason: err.Error()}
	}

	if inference.Skip {
		tb.line(fmt.Sprintf("skipped — %s", inference.Reason))
		tb.flush(deps.Trace)
		return Result{
			Action:    ActionDroppedNoSignal,
			Reason:    inference.Reason,
			Inference: inference,
		}
	}

	tb.line(fmt.Sprintf("llm: valence=%d labels=%v conf=%.2f",
		inference.Valence, inference.Labels, inference.Confidence))
	tb.flush(deps.Trace)

	// 2. Vocab filter — drop hallucinated labels/associations.
	before := len(inference.Labels) + len(inference.Associations)
	inference.Labels = filterKnown(inference.Labels, deps.Vocab.IsLabel)
	inference.Associations = filterKnown(inference.Associations, deps.Vocab.IsAssociation)
	if len(inference.Labels) == 0 {
		tb.line(fmt.Sprintf("vocab filter dropped all labels (had %d)", before))
		tb.flush(deps.Trace)
		return Result{
			Action:    ActionDroppedVocab,
			Reason:    fmt.Sprintf("no valid labels survived vocab filter (had %d entries)", before),
			Inference: inference,
		}
	}

	// 3. Valence must land in [1,7]; otherwise the model made
	// something up.
	if inference.Valence < 1 || inference.Valence > 7 {
		tb.line(fmt.Sprintf("valence %d out of range — dropped", inference.Valence))
		tb.flush(deps.Trace)
		return Result{
			Action:    ActionDroppedNoSignal,
			Reason:    fmt.Sprintf("valence %d out of range", inference.Valence),
			Inference: inference,
		}
	}

	// 4. Hybrid confidence = max(LLM self-rated, signal-heuristic score).
	heuristic := ScoreSignals(turns)
	confidence := inference.Confidence
	if heuristic > confidence {
		confidence = heuristic
	}
	tb.line(fmt.Sprintf("hybrid confidence: %.2f (llm %.2f, signals %.2f)",
		confidence, inference.Confidence, heuristic))
	tb.flush(deps.Trace)

	if confidence < cfg.ConfidenceLow {
		tb.line(fmt.Sprintf("below low threshold %.2f — dropped", cfg.ConfidenceLow))
		tb.flush(deps.Trace)
		return Result{
			Action:     ActionDroppedLow,
			Reason:     fmt.Sprintf("confidence %.2f below low threshold %.2f", confidence, cfg.ConfidenceLow),
			Confidence: confidence,
			Inference:  inference,
		}
	}

	// 5. Classifier fail-open check ("is this a real first-person
	// mood?").
	if deps.Classifier != nil {
		ok, reason := classifyReal(ctx, deps.Classifier, inference, turns)
		if !ok {
			tb.line(fmt.Sprintf("classifier rejected: %s", reason))
			tb.flush(deps.Trace)
			return Result{
				Action:     ActionDroppedClassify,
				Reason:     reason,
				Confidence: confidence,
				Inference:  inference,
			}
		}
		tb.line("classifier: REAL ✓")
		tb.flush(deps.Trace)
	}

	// 6. Build the candidate entry (not yet saved). Embedding is
	// computed once so both the dedup lookup and the SaveMoodEntry
	// see the same vector.
	candidate := &memory.MoodEntry{
		Timestamp:      deps.Clock(),
		Kind:           memory.MoodKindMomentary,
		Valence:        inference.Valence,
		Labels:         inference.Labels,
		Associations:   inference.Associations,
		Note:           inference.Note,
		Confidence:     confidence,
		ConversationID: deps.ConversationID,
	}

	// Embed text is the note plus the joined labels — same string that
	// SaveMoodEntry mirrors into vec_moods, so dedup and save see the
	// same vector geometry.
	embedText := candidate.Note
	if len(candidate.Labels) > 0 {
		embedText = embedText + " " + strings.Join(candidate.Labels, " ")
	}
	if deps.Embed != nil {
		vec, err := deps.Embed(ctx, embedText)
		if err != nil {
			log.Warn("mood agent embedding failed; proceeding without it",
				"err", err)
		} else {
			candidate.Embedding = vec
		}
	}

	// 7. Dedup + update — only when we have an embedding + vec_moods.
	//
	// Three tiers based on similarity to the nearest recent entry:
	//   ≥ DedupSimilarity (0.80)  → drop (nearly identical)
	//   ≥ UpdateSimilarity (0.55) → update existing entry in place
	//                                (same emotional arc, more detail)
	//   < UpdateSimilarity        → new entry (different mood)
	//
	// The update path also checks valence drift: if the new mood is
	// more than ±UpdateMaxValenceDrift away from the neighbor, the
	// emotional state has genuinely shifted and deserves its own row
	// even if the topic is similar.
	if len(candidate.Embedding) > 0 {
		neighbors, err := deps.Store.SimilarMoodEntriesWithin(
			deps.Clock(), candidate.Embedding, cfg.DedupWindow, 3,
		)
		if err != nil {
			log.Warn("dedup KNN query failed; proceeding", "err", err)
		} else if len(neighbors) > 0 {
			// sqlite-vec returns cosine DISTANCE (0 = identical).
			// Our thresholds are in similarity terms. Convert once.
			nearest := neighbors[0]
			topSim := 1.0 - nearest.Distance
			valenceDrift := candidate.Valence - nearest.Valence
			if valenceDrift < 0 {
				valenceDrift = -valenceDrift
			}

			if topSim >= cfg.DedupSimilarity {
				// Tier 1: nearly identical — drop.
				tb.line(fmt.Sprintf("dedup: similar to #%d (sim=%.2f) — dropped",
					nearest.ID, topSim))
				tb.flush(deps.Trace)
				return Result{
					Action:     ActionDroppedDedup,
					Reason:     fmt.Sprintf("similar entry #%d within window (sim=%.2f)", nearest.ID, topSim),
					Confidence: confidence,
					Inference:  inference,
				}
			}

			// Label overlap: does the new mood share at least one
			// emotional label with the existing entry? Without overlap,
			// the feeling has qualitatively shifted even if the topic
			// is similar (e.g., Frustrated→Relieved about the same event).
			labelOverlap := countLabelOverlap(candidate.Labels, nearest.Labels)

			if topSim >= cfg.UpdateSimilarity && valenceDrift <= cfg.UpdateMaxValenceDrift && labelOverlap > 0 {
				// Tier 2: same emotional territory, evolving — update
				// the existing entry with richer detail.
				candidate.Source = memory.MoodSourceInferred
				if err := deps.Store.UpdateMoodEntry(nearest.ID, candidate); err != nil {
					log.Error("mood update failed; falling through to new entry",
						"existing_id", nearest.ID, "err", err)
				} else {
					candidate.ID = nearest.ID
					tb.line(fmt.Sprintf("♻️ updated #%d (sim=%.2f, valence %d→%d)",
						nearest.ID, topSim, nearest.Valence, candidate.Valence))
					tb.flush(deps.Trace)
					log.Info("mood updated existing entry",
						"id", nearest.ID, "valence", candidate.Valence,
						"labels", candidate.Labels, "sim", topSim)
					return Result{
						Action:     ActionUpdated,
						Reason:     fmt.Sprintf("refined entry #%d (sim=%.2f)", nearest.ID, topSim),
						Confidence: confidence,
						Inference:  inference,
						Entry:      candidate,
					}
				}
			}

			// Tier 3: different enough — fall through to save new.
			tb.line(fmt.Sprintf("dedup: nearest #%d sim=%.2f drift=%d overlap=%d (new entry)",
				nearest.ID, topSim, valenceDrift, labelOverlap))
			tb.flush(deps.Trace)
		} else {
			tb.line("dedup: no neighbors in window")
			tb.flush(deps.Trace)
		}
	}

	// 8. Route by confidence tier.
	if confidence >= cfg.ConfidenceHigh {
		res := autoLog(deps, candidate, inference, confidence)
		if res.Entry != nil {
			tb.line(fmt.Sprintf("✅ auto-logged #%d (source=inferred)", res.Entry.ID))
		} else {
			tb.line(fmt.Sprintf("⚠️ auto-log errored: %s", res.Reason))
		}
		tb.flush(deps.Trace)
		return res
	}
	res := emitProposal(ctx, deps, cfg, candidate, inference, confidence)
	switch res.Action {
	case ActionProposalEmitted:
		tb.line("📩 medium-confidence proposal sent")
	case ActionErrored:
		tb.line(fmt.Sprintf("⚠️ proposal errored: %s", res.Reason))
	default:
		tb.line(fmt.Sprintf("dropped: %s", res.Reason))
	}
	tb.flush(deps.Trace)
	return res
}

// autoLog saves a high-confidence inferred entry and returns the
// Result the caller can assert on.
func autoLog(deps Deps, entry *memory.MoodEntry, inf *Inference, conf float64) Result {
	entry.Source = memory.MoodSourceInferred
	id, err := deps.Store.SaveMoodEntry(entry)
	if err != nil {
		log.Error("SaveMoodEntry failed during auto-log", "err", err)
		return Result{
			Action:     ActionErrored,
			Reason:     err.Error(),
			Confidence: conf,
			Inference:  inf,
		}
	}
	entry.ID = id
	log.Info("mood auto-logged",
		"id", id, "valence", entry.Valence,
		"labels", entry.Labels, "confidence", conf)
	return Result{
		Action:     ActionAutoLogged,
		Confidence: conf,
		Inference:  inf,
		Entry:      entry,
	}
}

// emitProposal hands a medium-confidence entry to the Telegram layer,
// stores the pending_mood_proposals row, and returns the Result.
// Dropped silently when deps.Propose is nil (e.g. tests that don't
// care about the proposal path).
func emitProposal(
	ctx context.Context,
	deps Deps,
	cfg AgentConfig,
	entry *memory.MoodEntry,
	inf *Inference,
	conf float64,
) Result {
	if deps.Propose == nil {
		return Result{
			Action:     ActionDroppedNoSignal,
			Reason:     "medium confidence but no Propose handler",
			Confidence: conf,
			Inference:  inf,
		}
	}

	expiresAt := deps.Clock().Add(cfg.ProposalExpiry)

	// Propose is allowed to panic (buggy extension, telebot edge case);
	// recover so the mood agent can't crash the post-reply goroutine.
	var chatID, msgID int64
	var proposeErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				proposeErr = fmt.Errorf("propose panic: %v", r)
			}
		}()
		chatID, msgID, proposeErr = deps.Propose(ctx, entry, expiresAt)
	}()
	if proposeErr != nil {
		return Result{
			Action:     ActionErrored,
			Reason:     proposeErr.Error(),
			Confidence: conf,
			Inference:  inf,
		}
	}

	// Store the pending proposal so the callback handler can resolve
	// it when the user taps.
	payload, err := json.Marshal(entry)
	if err != nil {
		return Result{
			Action:     ActionErrored,
			Reason:     fmt.Sprintf("marshal proposal: %v", err),
			Confidence: conf,
			Inference:  inf,
		}
	}

	p := &memory.PendingMoodProposal{
		Timestamp:         deps.Clock(),
		TelegramChatID:    chatID,
		TelegramMessageID: msgID,
		ProposalJSON:      payload,
		Status:            memory.MoodProposalPending,
		ExpiresAt:         expiresAt,
	}
	id, err := deps.Store.SavePendingMoodProposal(p)
	if err != nil {
		return Result{
			Action:     ActionErrored,
			Reason:     fmt.Sprintf("save pending proposal: %v", err),
			Confidence: conf,
			Inference:  inf,
		}
	}
	p.ID = id

	log.Info("mood proposal emitted",
		"proposal_id", id, "msg_id", msgID,
		"valence", entry.Valence, "confidence", conf)

	return Result{
		Action:     ActionProposalEmitted,
		Confidence: conf,
		Inference:  inf,
		Entry:      entry,
		Proposal:   p,
	}
}

// filterKnown keeps only strings for which isKnown returns true.
// Used to drop hallucinated labels / associations before they reach
// the DB.
func filterKnown(in []string, isKnown func(string) bool) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if isKnown(s) {
			out = append(out, s)
		}
	}
	return out
}

// countLabelOverlap returns how many labels appear in both slices.
// Used by the update path to distinguish "same feeling, more detail"
// from "different feeling about the same topic." Zero overlap means
// the emotional quality has shifted and a new entry is warranted.
func countLabelOverlap(a, b []string) int {
	set := make(map[string]bool, len(b))
	for _, s := range b {
		set[s] = true
	}
	count := 0
	for _, s := range a {
		if set[s] {
			count++
		}
	}
	return count
}

// trimToCurrentSession scans turns (chronological order) backwards for
// the last time gap exceeding the threshold. If found, returns only the
// turns after the gap — the "current session." If no gap exceeds the
// threshold, all turns are returned.
//
// This prevents stale emotional context from a previous session (e.g.,
// a heavy conversation last night) from bleeding into a fresh greeting
// this morning. The mood agent should only infer from the current
// conversational session, not yesterday's emotional state.
func trimToCurrentSession(turns []Turn, gap time.Duration) []Turn {
	if len(turns) < 2 || gap <= 0 {
		return turns
	}
	// Walk backwards — we want the LAST gap, which marks the start
	// of the current session.
	for i := len(turns) - 1; i > 0; i-- {
		prev := turns[i-1].Timestamp
		curr := turns[i].Timestamp
		if !prev.IsZero() && !curr.IsZero() && curr.Sub(prev) >= gap {
			return turns[i:]
		}
	}
	return turns
}

// errResult wraps a pre-dispatch failure so callers still get a
// Result back (Go convention: never panic from RunAgent, always
// return something the caller can log/branch on).
func errResult(err error) Result {
	return Result{Action: ActionErrored, Reason: err.Error()}
}

// traceBuf accumulates trace lines as the agent makes decisions.
// Each flush sends the full cumulative text to the callback so the
// Telegram message edit shows a growing list rather than one-liner
// overwrites. Safe to use even when the callback is nil — line() is
// a noop in that case. Not thread-safe: the agent runs single-
// threaded within one RunAgent call.
type traceBuf struct {
	header string
	lines  []string
}

// emit writes the first (header + body) line, initializing the buf.
// Subsequent calls should use line() + flush().
func (t *traceBuf) emit(cb func(string) error, body string) {
	t.header = body
	if cb == nil {
		return
	}
	_ = cb(body)
}

// line appends a detail line to the buffer. No callback fire — pair
// with flush().
func (t *traceBuf) line(s string) {
	t.lines = append(t.lines, s)
}

// flush renders the full header + lines and sends to the callback.
// Pair with line() to coalesce multiple decisions into one edit.
func (t *traceBuf) flush(cb func(string) error) {
	if cb == nil {
		return
	}
	text := t.header
	if len(t.lines) > 0 {
		text = text + "\n" + joinLines(t.lines)
	}
	_ = cb(text)
}

// joinLines joins with newlines. Out of line only so the agent file
// doesn't need another strings.Join import line in every caller.
func joinLines(in []string) string {
	if len(in) == 0 {
		return ""
	}
	out := in[0]
	for _, s := range in[1:] {
		out += "\n" + s
	}
	return out
}

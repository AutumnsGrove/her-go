package mood

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"her/llm"
	"her/logger"
	"her/memory"
)

var log = logger.WithPrefix("mood")

// Action describes what the agent did with a given turn. One action
// per RunAgent call; result.Action tells the caller exactly what
// happened so tests and the TUI can assert on it.
type Action string

const (
	ActionAutoLogged      Action = "auto_logged"
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

	// ProposalExpiry is how long a medium-confidence proposal stays
	// tappable before the sweeper edits it to "expired". Default 30m.
	ProposalExpiry time.Duration
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
		c.DedupSimilarity = 0.80
	}
	if c.ProposalExpiry <= 0 {
		c.ProposalExpiry = 30 * time.Minute
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
	Store *memory.Store

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

	// Trace collector — accumulates HTML lines and flushes to the
	// callback on every meaningful step. Safe to call even when the
	// callback is nil; the append is harmless noise.
	var trace traceBuf
	trace.emit(deps.Trace, "🎭 <b>mood</b>\nthinking…")

	// 1. Ask the LLM.
	inference, err := callLLM(ctx, deps.LLM, deps.Vocab, turns)
	if err != nil {
		log.Error("mood agent LLM call failed", "err", err)
		trace.line(fmt.Sprintf("⚠️ llm error: %s", err))
		trace.flush(deps.Trace)
		return Result{Action: ActionErrored, Reason: err.Error()}
	}

	if inference.Skip {
		trace.line(fmt.Sprintf("skipped — %s", inference.Reason))
		trace.flush(deps.Trace)
		return Result{
			Action:    ActionDroppedNoSignal,
			Reason:    inference.Reason,
			Inference: inference,
		}
	}

	trace.line(fmt.Sprintf("llm: valence=%d labels=%v conf=%.2f",
		inference.Valence, inference.Labels, inference.Confidence))
	trace.flush(deps.Trace)

	// 2. Vocab filter — drop hallucinated labels/associations.
	before := len(inference.Labels) + len(inference.Associations)
	inference.Labels = filterKnown(inference.Labels, deps.Vocab.IsLabel)
	inference.Associations = filterKnown(inference.Associations, deps.Vocab.IsAssociation)
	if len(inference.Labels) == 0 {
		trace.line(fmt.Sprintf("vocab filter dropped all labels (had %d)", before))
		trace.flush(deps.Trace)
		return Result{
			Action:    ActionDroppedVocab,
			Reason:    fmt.Sprintf("no valid labels survived vocab filter (had %d entries)", before),
			Inference: inference,
		}
	}

	// 3. Valence must land in [1,7]; otherwise the model made
	// something up.
	if inference.Valence < 1 || inference.Valence > 7 {
		trace.line(fmt.Sprintf("valence %d out of range — dropped", inference.Valence))
		trace.flush(deps.Trace)
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
	trace.line(fmt.Sprintf("hybrid confidence: %.2f (llm %.2f, signals %.2f)",
		confidence, inference.Confidence, heuristic))
	trace.flush(deps.Trace)

	if confidence < cfg.ConfidenceLow {
		trace.line(fmt.Sprintf("below low threshold %.2f — dropped", cfg.ConfidenceLow))
		trace.flush(deps.Trace)
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
			trace.line(fmt.Sprintf("classifier rejected: %s", reason))
			trace.flush(deps.Trace)
			return Result{
				Action:     ActionDroppedClassify,
				Reason:     reason,
				Confidence: confidence,
				Inference:  inference,
			}
		}
		trace.line("classifier: REAL ✓")
		trace.flush(deps.Trace)
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

	// 7. Dedup — only when we have an embedding + vec_moods available.
	if len(candidate.Embedding) > 0 {
		neighbors, err := deps.Store.SimilarMoodEntriesWithin(
			deps.Clock(), candidate.Embedding, cfg.DedupWindow, 3,
		)
		if err != nil {
			log.Warn("dedup KNN query failed; proceeding", "err", err)
		} else if len(neighbors) > 0 {
			// sqlite-vec returns cosine DISTANCE (0 = identical).
			// Our threshold is in similarity terms (0.80 = "this
			// similar or more"). Convert once to keep the config
			// human-readable.
			topSim := 1.0 - neighbors[0].Distance
			if topSim >= cfg.DedupSimilarity {
				trace.line(fmt.Sprintf("dedup: similar to #%d (sim=%.2f) — dropped",
					neighbors[0].ID, topSim))
				trace.flush(deps.Trace)
				return Result{
					Action:     ActionDroppedDedup,
					Reason:     fmt.Sprintf("similar entry #%d within window (sim=%.2f)", neighbors[0].ID, topSim),
					Confidence: confidence,
					Inference:  inference,
				}
			}
			trace.line(fmt.Sprintf("dedup: nearest #%d sim=%.2f (below %.2f threshold)",
				neighbors[0].ID, topSim, cfg.DedupSimilarity))
			trace.flush(deps.Trace)
		} else {
			trace.line("dedup: no neighbors in window")
			trace.flush(deps.Trace)
		}
	}

	// 8. Route by confidence tier.
	if confidence >= cfg.ConfidenceHigh {
		res := autoLog(deps, candidate, inference, confidence)
		if res.Entry != nil {
			trace.line(fmt.Sprintf("✅ auto-logged #%d (source=inferred)", res.Entry.ID))
		} else {
			trace.line(fmt.Sprintf("⚠️ auto-log errored: %s", res.Reason))
		}
		trace.flush(deps.Trace)
		return res
	}
	res := emitProposal(ctx, deps, cfg, candidate, inference, confidence)
	switch res.Action {
	case ActionProposalEmitted:
		trace.line("📩 medium-confidence proposal sent")
	case ActionErrored:
		trace.line(fmt.Sprintf("⚠️ proposal errored: %s", res.Reason))
	default:
		trace.line(fmt.Sprintf("dropped: %s", res.Reason))
	}
	trace.flush(deps.Trace)
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

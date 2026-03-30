package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"her/embed"
	"her/skills/loader"
	"her/tools"
)

// execFindSkill handles the find_skill tool call. It searches the skill
// registry using KNN cosine similarity over description embeddings.
//
// The agent calls this with a natural language query like "get weather
// forecast" and gets back matching skills ranked by relevance. The agent
// then decides which one to call (or none, if nothing fits).
func execFindSkill(argsJSON string, tctx *tools.Context) string {
	if tctx.SkillRegistry == nil {
		return "no skills available — skill registry not initialized"
	}

	var args struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing find_skill args: %s", err)
	}
	if args.Query == "" {
		return "error: query is required"
	}

	// Search with reasonable defaults: top 5 results, minimum 0.3 score.
	// 0.3 is intentionally low — we'd rather show marginal matches than
	// miss something useful. The agent can judge relevance from scores.
	results, err := tctx.SkillRegistry.Find(args.Query, 5, 0.3)
	if err != nil {
		return fmt.Sprintf("error searching skills: %s", err)
	}

	if len(results) == 0 {
		return "no matching skills found"
	}

	// Format results for the agent. Include name, description, score,
	// and parameter summary so the agent knows how to call the skill.
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d matching skill(s):\n\n", len(results)))

	for _, r := range results {
		sb.WriteString(fmt.Sprintf("**%s** (score: %.2f)\n", r.Skill.Name, r.Score))
		sb.WriteString(fmt.Sprintf("  %s\n", r.Skill.Description))

		// Show parameter summary so the agent knows what to pass.
		if len(r.Skill.Params) > 0 {
			sb.WriteString("  params:\n")
			for _, p := range r.Skill.Params {
				req := ""
				if p.Required {
					req = " (required)"
				}
				sb.WriteString(fmt.Sprintf("    - %s (%s)%s: %s\n", p.Name, p.Type, req, p.Description))
			}
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

// execRunSkill handles the run_skill tool call. It looks up the skill by
// name, executes it via the runner, and returns the result.
//
// The skill binary receives args as JSON on stdin and writes its result
// to stdout. The runner handles compilation (Go), timeouts, and
// environment sandboxing.
func execRunSkill(argsJSON string, tctx *tools.Context) string {
	if tctx.SkillRegistry == nil {
		return "no skills available — skill registry not initialized"
	}

	var args struct {
		Name string         `json:"name"`
		Args map[string]any `json:"args"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing run_skill args: %s", err)
	}
	if args.Name == "" {
		return "error: skill name is required"
	}

	// Look up the skill.
	skill := tctx.SkillRegistry.Get(args.Name)
	if skill == nil {
		// Suggest using find_skill — common mistake is to guess a name.
		available := tctx.SkillRegistry.List()
		if len(available) > 0 {
			return fmt.Sprintf("unknown skill %q. Available: %s. Use find_skill to search by intent.",
				args.Name, strings.Join(available, ", "))
		}
		return fmt.Sprintf("unknown skill %q — no skills are registered", args.Name)
	}

	// Default to empty args if none provided.
	if args.Args == nil {
		args.Args = make(map[string]any)
	}

	// --- Mood dedup gate ---
	// Before running log_mood, check if a semantically similar mood was
	// logged recently. This mirrors how save_fact rejects duplicate facts
	// via embedding similarity. Without this, the agent tends to log the
	// same mood on every message in an emotional conversation.
	if skill.Name == "log_mood" && tctx.Store != nil && tctx.EmbedClient != nil {
		if dup := checkMoodDuplicate(args.Args, tctx); dup != "" {
			return dup
		}
	}

	// --- Classifier gate for mood ---
	// Check if the mood is about the real user or a fictional character.
	// Runs after dedup (no point classifying a duplicate) and before
	// execution (we can't intercept the DB write inside the skill).
	if skill.Name == "log_mood" && tctx.ClassifierLLM != nil {
		moodNote, _ := args.Args["note"].(string)
		if moodNote != "" {
			snippet, _ := tctx.Store.RecentMessages(tctx.ConversationID, 3)
			verdict := classifyMemoryWrite(tctx.ClassifierLLM, "mood", moodNote, snippet)
			if !verdict.Allowed {
				return rejectionMessage(verdict)
			}
		}
	}

	// Update status so the user sees what's happening.
	if tctx.StatusCallback != nil {
		tctx.StatusCallback(fmt.Sprintf("running %s...", skill.Name))
	}

	log.Info("running skill", "name", skill.Name)
	result, err := loader.Run(skill, args.Args)
	if err != nil {
		return fmt.Sprintf("error running skill %s: %s", skill.Name, err)
	}

	log.Info("skill finished", "name", skill.Name, "duration", result.Duration)

	// Format the result for the agent.
	if result.Error != "" {
		if result.Output != nil {
			// Skill wrote error JSON before exiting (via skillkit.Error).
			return fmt.Sprintf("skill %s error: %s\noutput: %s", skill.Name, result.Error, string(result.Output))
		}
		return fmt.Sprintf("skill %s error: %s", skill.Name, result.Error)
	}

	if result.Output != nil {
		return string(result.Output)
	}
	if result.RawOut != "" {
		return fmt.Sprintf("(non-JSON output) %s", result.RawOut)
	}

	return "skill completed with no output"
}

// execSearchHistory handles the search_history tool call. It searches a
// skill's sidecar database for past execution results that match the
// query, using KNN semantic search.
//
// This lets the agent check "did I already search for this?" before
// re-running a skill. Past results include freshness metadata so the
// agent can judge whether to reuse a cached result or run fresh.
func execSearchHistory(argsJSON string, tctx *tools.Context) string {
	if tctx.SkillRegistry == nil {
		return "no skills available — skill registry not initialized"
	}
	if tctx.EmbedClient == nil {
		return "search_history unavailable — embedding client not configured"
	}

	var args struct {
		SkillName string `json:"skill_name"`
		Query     string `json:"query"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing search_history args: %s", err)
	}
	if args.SkillName == "" || args.Query == "" {
		return "error: both skill_name and query are required"
	}

	// Look up the skill.
	skill := tctx.SkillRegistry.Get(args.SkillName)
	if skill == nil {
		return fmt.Sprintf("unknown skill %q", args.SkillName)
	}

	// 4th-party skills have no sidecar access.
	if skill.TrustLevel == loader.TrustFourthParty {
		return "no history available for this skill (4th-party, unvetted)"
	}

	// Embed the query.
	queryVec, err := tctx.EmbedClient.Embed(args.Query)
	if err != nil {
		return fmt.Sprintf("error embedding query: %s", err)
	}

	// Open the sidecar DB and search.
	sdb, err := loader.OpenSidecar(skill, tctx.EmbedClient.Dimension)
	if err != nil {
		return fmt.Sprintf("no execution history for %s", args.SkillName)
	}
	defer sdb.Close()

	results, err := sdb.SearchHistory(queryVec, 5)
	if err != nil {
		return fmt.Sprintf("error searching history: %s", err)
	}

	if len(results) == 0 {
		return fmt.Sprintf("no past executions found for %s matching %q", args.SkillName, args.Query)
	}

	// Format results for the agent.
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d cached result(s) for %s:\n\n", len(results), args.SkillName))

	for i, r := range results {
		status := "OK"
		if r.ExitCode != 0 {
			status = "ERROR"
		}
		sb.WriteString(fmt.Sprintf("%d. [%s] %s (took %s)\n", i+1, status, r.Age, r.Duration))
		sb.WriteString(fmt.Sprintf("   args: %s\n", r.Args))
		sb.WriteString(fmt.Sprintf("   result: %s\n\n", r.Result))
	}

	return sb.String()
}

// moodDedupWindowMinutes is how far back we look for duplicate moods.
// 120 minutes (2 hours) means: if the user said "feeling stuck" an hour
// ago, we won't log "feeling stuck and restless" as a separate entry.
const moodDedupWindowMinutes = 120

// moodSimilarityThreshold is the cosine similarity above which two mood
// notes are considered duplicates. 0.75 is intentionally lower than the
// fact threshold (0.85) because mood notes tend to be short and similar
// phrasing should be caught more aggressively.
const moodSimilarityThreshold = 0.75

// checkMoodDuplicate checks whether a proposed mood entry is too similar
// to a recently logged one. Returns an explanatory string if the mood
// should be skipped, or empty string if it's OK to log.
//
// Two-tier check:
//  1. Time gate — any mood in the last 30 minutes is a duplicate regardless
//     of content (the user's emotional state doesn't change that fast).
//  2. Semantic gate — any mood in the last 2 hours with a similar note
//     (cosine similarity >= 0.75) is a duplicate.
func checkMoodDuplicate(skillArgs map[string]any, tctx *tools.Context) string {
	note, _ := skillArgs["note"].(string)

	// Tier 1: time gate — if ANY mood was logged in the last 30 minutes,
	// skip this one. The agent shouldn't be logging mood multiple times
	// per conversation exchange.
	recentNotes, err := tctx.Store.RecentMoodNotes(30)
	if err != nil {
		log.Warn("mood dedup: couldn't check recent moods", "err", err)
		return "" // fail open — let it through
	}
	if len(recentNotes) > 0 {
		log.Info("mood dedup: skipped (mood already logged in last 30 min)",
			"recent_note", recentNotes[0], "proposed_note", note)
		return fmt.Sprintf("mood already logged in the last 30 minutes (%q) — skipping to avoid duplicates", recentNotes[0])
	}

	// Tier 2: semantic gate — check the last 2 hours for similar notes.
	if note == "" {
		return "" // no note to compare
	}
	windowNotes, err := tctx.Store.RecentMoodNotes(moodDedupWindowMinutes)
	if err != nil {
		log.Warn("mood dedup: couldn't check window moods", "err", err)
		return ""
	}
	if len(windowNotes) == 0 {
		return "" // no moods in window — all clear
	}

	// Embed the proposed note.
	newVec, err := tctx.EmbedClient.Embed(note)
	if err != nil {
		log.Warn("mood dedup: embed failed", "err", err)
		return "" // fail open
	}

	// Compare against each recent note.
	for _, existing := range windowNotes {
		existVec, err := tctx.EmbedClient.Embed(existing)
		if err != nil {
			continue
		}
		sim := embed.CosineSimilarity(newVec, existVec)
		if sim >= moodSimilarityThreshold {
			log.Info("mood dedup: skipped (semantically similar to recent mood)",
				"similarity", fmt.Sprintf("%.3f", sim),
				"existing", existing, "proposed", note)
			return fmt.Sprintf("mood too similar to recent entry %q (%.0f%% match) — skipping", existing, sim*100)
		}
	}

	return "" // no duplicates found
}

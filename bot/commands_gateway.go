// Package bot — transport-neutral command implementations.
//
// Each Exec* method contains the business logic for a slash command,
// returning a plain-text result instead of sending via tele.Context.
// The gateway builds CommandDefs that call these methods, making every
// command available on all adapters (Gradio, Telegram, future Discord, etc.).
//
// Telegram's handleMessage intercepts /commands and routes them here
// too — the old per-command telebot registrations are gone for migrated
// commands. One command system for everything.
package bot

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"encoding/json"

	"her/compact"
	"her/memory"
	"her/mood"
	"her/persona"
	"her/scheduler"

	"gopkg.in/yaml.v3"
)

// helpYAML is loaded from help.yaml — the single source of truth for
// /help output. Uses {{her}} placeholders expanded at render time.
//
//go:embed help.yaml
var helpYAML string

// helpSpec mirrors the YAML structure in help.yaml.
type helpSpec struct {
	Sections []struct {
		Title    string `yaml:"title"`
		Commands []struct {
			Cmd  string `yaml:"cmd"`
			Desc string `yaml:"desc"`
		} `yaml:"commands"`
	} `yaml:"sections"`
	Footer string `yaml:"footer"`
}

// GatewayCommand pairs a name with its handler for the in-process
// command router. Populated by RegisterGatewayCommands.
type GatewayCommand struct {
	Name    string
	Handler func(ctx context.Context, args string) (string, error)
}

// RegisterGatewayCommands stores command handlers that handleMessage
// will check before falling through to the agent pipeline. Called by
// the Telegram adapter after gateway command registration.
func (b *Bot) RegisterGatewayCommands(cmds []GatewayCommand) {
	b.gatewayCmds = cmds
}

// tryGatewayCommand checks if a message is a registered gateway command
// and handles it. Returns the response text and true if handled, or
// ("", false) if the message should fall through to the pipeline.
// Transport-agnostic — the caller handles sending the result.
func (b *Bot) tryGatewayCommand(text string, chatID int64) (string, bool) {
	if !strings.HasPrefix(text, "/") {
		return "", false
	}

	parts := strings.SplitN(text, " ", 2)
	cmdName := strings.TrimPrefix(parts[0], "/")
	args := ""
	if len(parts) > 1 {
		args = parts[1]
	}

	convID := b.getConversationID(chatID)

	// /clear is adapter-specific — it resets the conversation ID.
	if cmdName == "clear" {
		b.store.LogCommand("/clear", chatID, convID, args)
		return b.ExecClear(chatID), true
	}

	// /compact needs the conversation ID from the chat.
	if cmdName == "compact" {
		b.store.LogCommand("/compact", chatID, convID, args)
		result, err := b.ExecCompact(convID)
		if err != nil {
			return fmt.Sprintf("Error: %v", err), true
		}
		return result, true
	}

	for _, cmd := range b.gatewayCmds {
		if cmd.Name == cmdName {
			b.store.LogCommand("/"+cmdName, chatID, convID, args)
			result, err := cmd.Handler(context.Background(), args)
			if err != nil {
				return fmt.Sprintf("Error: %v", err), true
			}
			return result, true
		}
	}

	return "", false
}

// ExecClear resets the conversation context for a given chat ID.
func (b *Bot) ExecClear(chatID int64) string {
	key := fmt.Sprintf("%d", chatID)
	newID := fmt.Sprintf("tg_%d_%d", chatID, time.Now().Unix())
	b.conversationIDs.Store(key, newID)
	log.Info("exec clear: conversation reset", "chat", chatID, "new_id", newID)
	return "Context cleared. Fresh start!"
}

// ExecHelp renders the help text as plain text (no HTML tags).
func (b *Bot) ExecHelp() string {
	var spec helpSpec
	if err := yaml.Unmarshal([]byte(helpYAML), &spec); err != nil {
		return "Something went wrong loading help."
	}

	expand := func(s string) string {
		return strings.ReplaceAll(s, "{{her}}", b.cfg.Identity.Her)
	}

	var msg strings.Builder
	msg.WriteString("== Commands ==\n\n")

	for _, section := range spec.Sections {
		msg.WriteString(section.Title)
		msg.WriteString("\n")
		for _, cmd := range section.Commands {
			fmt.Fprintf(&msg, "  %s — %s\n", cmd.Cmd, expand(cmd.Desc))
		}
		msg.WriteString("\n")
	}

	if spec.Footer != "" {
		msg.WriteString(expand(strings.TrimSpace(spec.Footer)))
	}

	return msg.String()
}

// ExecStats returns aggregate usage statistics as plain text.
func (b *Bot) ExecStats() (string, error) {
	stats, err := b.store.GetStats()
	if err != nil {
		return "", fmt.Errorf("couldn't load stats: %w", err)
	}

	var cmdSection string
	if stats.TotalCommands > 0 {
		cmdSection = fmt.Sprintf("\nCommands: %d total\n", stats.TotalCommands)
		for _, cc := range stats.CommandCounts {
			cmdSection += fmt.Sprintf("  %s: %d\n", cc.Command, cc.Count)
		}
	}

	return fmt.Sprintf(
		"== Stats ==\n\n"+
			"Messages: %d total (%d you, %d me)\n"+
			"Active days: %d\n\n"+
			"Memory: %d facts (%d about you, %d about me)\n\n"+
			"Tokens: %s total\n"+
			"  Chat: %s ($%.4f)\n"+
			"  Agent: %s ($%.4f)\n"+
			"Total cost: $%.4f\n"+
			"Avg latency: %dms%s",
		stats.TotalMessages, stats.UserMessages, stats.MiraMessages,
		stats.ConversationDays,
		stats.TotalFacts, stats.UserFacts, stats.SelfFacts,
		formatTokens(stats.TotalTokens),
		formatTokens(stats.ChatTokens), stats.ChatCostUSD,
		formatTokens(stats.AgentTokens), stats.AgentCostUSD,
		stats.TotalCostUSD,
		stats.AvgLatencyMs,
		cmdSection,
	), nil
}

// ExecFacts returns all active memories grouped by subject.
func (b *Bot) ExecFacts() (string, error) {
	memories, err := b.store.AllActiveMemories()
	if err != nil {
		return "", fmt.Errorf("couldn't load memories: %w", err)
	}

	if len(memories) == 0 {
		return "No memories saved yet. Keep chatting!", nil
	}

	var msg strings.Builder
	msg.WriteString("== What I Know ==\n\n")

	currentSubject := ""
	for _, m := range memories {
		if m.Subject != currentSubject {
			currentSubject = m.Subject
			if currentSubject == "user" {
				msg.WriteString("About you:\n")
			} else {
				msg.WriteString("\nAbout me:\n")
			}
		}
		msg.WriteString(fmt.Sprintf("  #%d [%s] %s\n", m.ID, m.Category, m.Content))
	}

	msg.WriteString("\nUse /forget <id> to remove a memory.")
	return msg.String(), nil
}

// ExecForget deactivates a memory by ID.
func (b *Bot) ExecForget(args string) (string, error) {
	args = strings.TrimSpace(args)
	if args == "" {
		return b.ExecFacts()
	}

	var factID int64
	if _, err := fmt.Sscanf(args, "%d", &factID); err != nil {
		return "Usage: /forget <fact_id>\n\nUse /facts to see all active facts with their IDs.", nil
	}

	if err := b.store.DeactivateMemory(factID); err != nil {
		return "", fmt.Errorf("couldn't forget memory %d: %w", factID, err)
	}

	log.Info("exec forget: deactivated memory", "memory_id", factID)
	return fmt.Sprintf("Done — forgot memory #%d.", factID), nil
}

// ExecTraces toggles agent thinking traces on/off.
func (b *Bot) ExecTraces() (string, error) {
	newState := !b.cfg.Driver.Trace
	if err := b.cfg.SetTrace(b.configPath, newState); err != nil {
		return "", fmt.Errorf("failed to update config: %w", err)
	}
	if newState {
		return "Agent traces enabled — you'll see thinking traces before each reply.", nil
	}
	return "Agent traces disabled.", nil
}

// ExecStatus returns the bot's current operational state.
func (b *Bot) ExecStatus() string {
	uptime := time.Since(b.startTime).Round(time.Second)
	stats, _ := b.store.GetStats()

	check := func(label string, ok bool) string {
		if ok {
			return label + ": on"
		}
		return label + ": off"
	}

	sttStatus := "off"
	if b.voiceClient != nil {
		if b.voiceClient.IsAvailable() {
			sttStatus = "running"
		} else {
			sttStatus = "not responding"
		}
	}
	ttsStatus := "off"
	if b.ttsClient != nil {
		if b.ttsClient.IsAvailable() {
			ttsStatus = "running"
		} else {
			ttsStatus = "not responding"
		}
	}

	managedBy := "manual (go run)"
	if mgr := b.processManager(); mgr != nil && mgr.IsManaged() {
		managedBy = mgr.Name()
	}

	return fmt.Sprintf(
		"== Status ==\n\n"+
			"Uptime: %s\n"+
			"Process: %s\n"+
			"Go: %s\n\n"+
			"Models:\n"+
			"  Chat: %s\n"+
			"  Agent: %s\n"+
			"  Vision: %s\n\n"+
			"Services:\n"+
			"  %s\n"+
			"  %s\n"+
			"  %s\n\n"+
			"Voice:\n"+
			"  STT (%s): %s\n"+
			"  TTS (Piper): %s\n\n"+
			"Session:\n"+
			"  Messages: %d\n"+
			"  Facts: %d\n"+
			"  Cost: $%.4f",
		uptime, managedBy, runtime.Version(),
		b.cfg.Chat.Model, b.cfg.Driver.Model, b.cfg.Vision.Model,
		check("Embeddings", b.embedClient != nil),
		check("Web search", b.tavilyClient != nil),
		check("Vision", b.visionLLM != nil),
		b.cfg.Voice.STT.Engine, sttStatus,
		ttsStatus,
		stats.TotalMessages, stats.TotalFacts, stats.TotalCostUSD,
	)
}

// ExecReflect triggers a manual reflection and returns the result.
func (b *Bot) ExecReflect() (string, error) {
	recent, err := b.store.GlobalRecentMessages(10)
	if err != nil || len(recent) < 2 {
		return "Not enough conversation history to reflect on yet. Keep chatting!", nil
	}

	memories, _ := b.store.RecentMemories("user", 10)
	selfMemories, _ := b.store.RecentMemories("self", 10)

	var factStrings []string
	for _, m := range memories {
		factStrings = append(factStrings, m.Content)
	}
	for _, m := range selfMemories {
		if m.Category != "reflection" {
			factStrings = append(factStrings, "(self) "+m.Content)
		}
	}

	if len(factStrings) == 0 {
		return "I don't have enough memories to reflect on yet. Let's keep talking!", nil
	}

	var lastUser, lastBot string
	for i := len(recent) - 1; i >= 0; i-- {
		if recent[i].Role == "user" && lastUser == "" {
			lastUser = recent[i].ContentRaw
		}
		if recent[i].Role == "assistant" && lastBot == "" {
			lastBot = recent[i].ContentRaw
		}
		if lastUser != "" && lastBot != "" {
			break
		}
	}

	err = persona.Reflect(b.llm, b.store, lastUser, lastBot, factStrings, b.cfg.Identity.Her, b.cfg.Identity.User)
	if err != nil {
		return "", fmt.Errorf("reflection failed: %w", err)
	}

	reflections, _ := b.store.ReflectionsSince(time.Now().Add(-10 * time.Second))
	if len(reflections) > 0 {
		return fmt.Sprintf("Reflection:\n\n%s", reflections[len(reflections)-1].Content), nil
	}

	return "Done reflecting. Use /facts to see what I wrote.", nil
}

// ExecReflections returns recent reflections as plain text.
func (b *Bot) ExecReflections() (string, error) {
	reflections, err := b.store.ReflectionsSince(time.Time{})
	if err != nil || len(reflections) == 0 {
		return "No reflections yet. Reflections happen after memory-dense conversations.", nil
	}

	start := len(reflections) - 5
	if start < 0 {
		start = 0
	}
	recent := reflections[start:]

	var msg strings.Builder
	msg.WriteString("== Recent Reflections ==\n\n")
	for i := len(recent) - 1; i >= 0; i-- {
		r := recent[i]
		ts := r.Timestamp.Format("Jan 2, 3:04 PM")
		text := r.Content
		if len(text) > 250 {
			text = text[:250] + "..."
		}
		fmt.Fprintf(&msg, "%s\n%s\n\n", ts, text)
	}

	fmt.Fprintf(&msg, "%d total reflections", len(reflections))
	return msg.String(), nil
}

// ExecPersona handles /persona and its subcommands (traits, history, rewrite).
func (b *Bot) ExecPersona(args string) (string, error) {
	args = strings.TrimSpace(args)

	switch args {
	case "traits":
		return b.execPersonaTraits()
	case "history":
		return b.execPersonaHistory()
	case "rewrite", "write":
		return b.execPersonaRewrite()
	default:
		data, err := os.ReadFile(b.cfg.Persona.PersonaFile)
		if err != nil || len(data) == 0 {
			return "No persona description yet. I'll develop one as we keep chatting!", nil
		}
		return fmt.Sprintf("== Who I Am Right Now ==\n\n%s", string(data)), nil
	}
}

func (b *Bot) execPersonaTraits() (string, error) {
	traits, err := b.store.GetCurrentTraits()
	if err != nil || len(traits) == 0 {
		return "No trait scores yet. Traits are extracted after persona rewrites.", nil
	}

	var msg strings.Builder
	msg.WriteString("== Personality Traits ==\n\n")

	for _, t := range traits {
		if t.TraitName == "humor_style" {
			fmt.Fprintf(&msg, "Humor style: %s\n", t.Value)
			continue
		}

		f := 0.0
		fmt.Sscanf(t.Value, "%f", &f)
		filled := int(f * 10)
		if filled > 10 {
			filled = 10
		}
		bar := strings.Repeat("█", filled) + strings.Repeat("░", 10-filled)

		displayName := strings.ToUpper(t.TraitName[:1]) + t.TraitName[1:]
		fmt.Fprintf(&msg, "%-11s %s %s\n", displayName, bar, t.Value)
	}

	fmt.Fprintf(&msg, "\nUpdated: persona v%d", traits[0].PersonaVersionID)
	return msg.String(), nil
}

func (b *Bot) execPersonaHistory() (string, error) {
	versions, err := b.store.PersonaHistory(5)
	if err != nil || len(versions) == 0 {
		return "No persona history yet.", nil
	}

	var msg strings.Builder
	msg.WriteString("== Persona History ==\n\n")
	for _, v := range versions {
		content := v.Content
		if len(content) > 150 {
			content = content[:150] + "..."
		}
		fmt.Fprintf(&msg, "v%d — %s\nTrigger: %s\n%s\n\n",
			v.ID, v.Timestamp.Format("Jan 2, 3:04 PM"), v.Trigger, content)
	}
	return msg.String(), nil
}

func (b *Bot) execPersonaRewrite() (string, error) {
	rewritten, err := persona.MaybeRewrite(b.llm, b.classifierLLM, b.embedClient, b.store, b.cfg.Persona.PersonaFile, b.cfg.Identity.Her)
	if err != nil {
		return "", fmt.Errorf("rewrite failed: %w", err)
	}
	if !rewritten {
		return "Rewrite ran but nothing changed.", nil
	}

	data, err := os.ReadFile(b.cfg.Persona.PersonaFile)
	if err != nil {
		return "Persona rewritten but couldn't read it back. Check persona.md.", nil
	}

	return fmt.Sprintf("Persona rewritten.\n\n%s", string(data)), nil
}

// ExecDream runs a full dream cycle and returns a summary.
func (b *Bot) ExecDream() (string, error) {
	var msg strings.Builder
	msg.WriteString("== Dream Complete ==\n\n")

	dreamLLM := b.dreamAgentLLM
	if dreamLLM == nil {
		dreamLLM = b.memoryAgentLLM
	}
	if dreamLLM != nil && b.cfg.Dream.DreamEnabled() {
		result := persona.RunMemoryDreamer(persona.MemoryDreamerParams{
			LLM:         dreamLLM,
			Store:       b.store,
			EmbedClient: b.embedClient,
			Cfg:         b.cfg,
		})
		if result.Error != nil {
			log.Error("dream consolidation", "err", result.Error)
			fmt.Fprintf(&msg, "Consolidation error: %v\n\n", result.Error)
		} else if result.Rewrites+result.Merges+result.Expires+result.Creates > 0 {
			fmt.Fprintf(&msg, "Consolidated: %d rewrites, %d merges, %d expires, %d creates\n\n",
				result.Rewrites, result.Merges, result.Expires, result.Creates)
		} else {
			msg.WriteString("Memories look tidy — nothing to consolidate.\n\n")
		}
	}

	if err := persona.NightlyReflect(b.llm, b.store, b.cfg, b.cfg.Identity.Her, b.cfg.Identity.User); err != nil {
		return "", fmt.Errorf("reflection failed: %w", err)
	}

	minDays := b.cfg.Persona.MinRewriteDays
	if minDays == 0 {
		minDays = 7
	}
	minRefl := b.cfg.Persona.MinReflections
	if minRefl == 0 {
		minRefl = 3
	}
	rewritten, err := persona.GatedRewrite(b.llm, b.classifierLLM, b.embedClient, b.store, b.cfg.Persona.PersonaFile, b.cfg.Identity.Her, true, minDays, minRefl)
	if err != nil {
		return "", fmt.Errorf("rewrite failed: %w", err)
	}

	reflections, _ := b.store.ReflectionsSince(time.Now().Add(-30 * time.Second))
	if len(reflections) > 0 {
		fmt.Fprintf(&msg, "Reflection:\n%s\n\n", reflections[len(reflections)-1].Content)
	} else {
		msg.WriteString("Nothing notable to reflect on right now.\n\n")
	}

	// Step 3: Tomorrow's preload.
	if b.cfg.Dream.TomorrowPreload.Enabled {
		if err := persona.RunTomorrowPreload(persona.TomorrowPreloadParams{
			LLM:      b.llm,
			Store:    b.store,
			Cfg:      b.cfg,
			EventBus: b.eventBus,
		}); err != nil {
			log.Error("dream preload", "err", err)
			fmt.Fprintf(&msg, "Preload error: %v\n\n", err)
		} else {
			msg.WriteString("Tomorrow's preload generated.\n\n")
		}
	}

	if rewritten {
		msg.WriteString("Persona rewritten. Use /persona to see the update.")
	} else {
		msg.WriteString("No persona changes — not enough has shifted yet.")
	}
	return msg.String(), nil
}

// ExecDreamLog returns recent memory dreamer audit entries.
func (b *Bot) ExecDreamLog() (string, error) {
	audits, err := b.store.RecentDreamAudits(10)
	if err != nil || len(audits) == 0 {
		return "No dream audit entries yet. Run /dream to trigger a consolidation cycle.", nil
	}

	var msg strings.Builder
	msg.WriteString("== Recent Dream Operations ==\n\n")
	for _, a := range audits {
		ts := a.Timestamp.Format("Jan 2, 3:04 PM")
		dryTag := ""
		if a.DryRun {
			dryTag = " [DRY RUN]"
		}
		afterPreview := a.AfterText
		if len(afterPreview) > 100 {
			afterPreview = afterPreview[:100] + "..."
		}
		fmt.Fprintf(&msg, "%s%s — %s\n%s\nIDs: %v → %d\n\n",
			a.Operation, dryTag, ts, afterPreview, a.SourceIDs, a.ResultID)
	}
	return msg.String(), nil
}

// ExecCompact triggers conversation compaction and returns the result.
func (b *Bot) ExecCompact(convID string) (string, error) {
	recent, err := b.store.RecentMessages(convID, b.cfg.Memory.RecentMessages)
	if err != nil || len(recent) < 4 {
		return "Not enough messages to compact yet.", nil
	}

	tokensBefore := compact.EstimateHistoryTokens("", recent)

	cr, err := compact.MaybeCompact(b.llm, b.store, convID, recent, 1, b.cfg.Identity.Her, b.cfg.Identity.User)
	if err != nil {
		return "", fmt.Errorf("compaction failed: %w", err)
	}

	tokensAfter := compact.EstimateHistoryTokens(cr.Summary, cr.KeptMessages)
	saved := tokensBefore - tokensAfter

	return fmt.Sprintf(
		"== Compacted ==\n\n"+
			"Messages: %d → %d kept\n"+
			"Tokens: ~%d → ~%d (saved ~%d)\n\n"+
			"Summary:\n%s",
		len(recent), len(cr.KeptMessages),
		tokensBefore, tokensAfter, saved,
		cr.Summary,
	), nil
}

// ExecLastTrace returns the last turn's full trace snapshot.
func (b *Bot) ExecLastTrace() string {
	b.lastTraceMu.Lock()
	snapshot := b.lastTraceSnapshot
	b.lastTraceMu.Unlock()

	if snapshot == "" {
		return "No trace available yet — send a message first (with /traces enabled)."
	}
	return snapshot
}

// ExecRollup forces a daily mood rollup. In production the scheduler
// fires this at 21:00; this command lets sims and manual testing
// trigger it on demand.
func (b *Bot) ExecRollup() (string, error) {
	noopSend := func(_ int64, text string) (int, error) { return 0, nil }
	deps := &scheduler.Deps{Store: b.store, Send: noopSend, ChatID: 1}

	before, _ := b.store.RecentMoodEntries(memory.MoodKindMomentary, 1)
	var beforeID int64
	if len(before) > 0 {
		beforeID = before[0].ID
	}

	h := mood.DailyRollupHandler()
	if err := h.Execute(context.Background(), json.RawMessage(`{}`), deps); err != nil {
		return "", fmt.Errorf("running rollup: %w", err)
	}

	after, _ := b.store.RecentMoodEntries(memory.MoodKindDaily, 1)
	if len(after) > 0 && after[0].ID != beforeID {
		entry := after[0]
		return fmt.Sprintf("Rollup logged entry #%d: valence=%d labels=%s",
			entry.ID, entry.Valence, entry.Labels), nil
	}
	return "Rollup ran — no new daily entry (already exists or insufficient data).", nil
}

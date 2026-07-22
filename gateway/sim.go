package gateway

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"her/config"
	"her/memory"
	"her/tui"
)

// simTurnTimeout bounds how long a single sim turn waits for a reply.
// Worker-delegated turns (research reports with multiple web_search/
// web_read/view_image calls) legitimately run long — 5 minutes was
// clipping real, still-progressing worker runs, not just hung ones.
const simTurnTimeout = 15 * time.Minute

// SimMessage is a single message in a simulation scenario.
// Supports three forms:
//   - "plain text"          → Text field
//   - image: path/to.jpg    → Image field
//   - advance_time: 2h      → AdvanceTime field
//   - text: "..."           → explicit Text + optional Image
type SimMessage struct {
	Text        string `yaml:"text"`
	Image       string `yaml:"image"`        // path to local image file (optional)
	AdvanceTime string `yaml:"advance_time"` // time-travel directive: "2h", "1d", "30m"
}

// UnmarshalYAML implements yaml.Unmarshaler to accept both plain strings
// and structured objects in the messages list. Uses node-based decoding
// to properly distinguish between scalar strings and mapping objects.
func (m *SimMessage) UnmarshalYAML(node *yaml.Node) error {
	// Fast path: plain string → text-only message.
	if node.Kind == yaml.ScalarNode {
		m.Text = node.Value
		return nil
	}

	// Slow path: mapping with text/image/advance_time fields.
	// Use a type alias to avoid infinite recursion — without this,
	// node.Decode(&m) would call UnmarshalYAML again forever.
	type rawMsg SimMessage
	var raw rawMsg
	if err := node.Decode(&raw); err != nil {
		return fmt.Errorf("decoding sim message: %w", err)
	}
	m.Text = raw.Text
	m.Image = raw.Image
	m.AdvanceTime = raw.AdvanceTime
	return nil
}

// SimTriggers defines lifecycle events to fire during a sim run.
type SimTriggers struct {
	CompactAfter  int   // force compaction after turn N (0 = disabled)
	DreamAfter    []int // run dream cycle after these turns
	RunDream      bool  // run dream after all messages complete
	RunRollup     bool  // force the daily mood rollup after all messages complete
	FireSchedules bool  // force-fire all user-created schedules after all messages
}

// SimOptions holds runtime options for sim execution.
type SimOptions struct {
	DelaySeconds int // pause between turns (0 = no delay)
}

// SimResult holds the response for one sim turn, enriched with
// structured data captured from the bus event stream.
// Can represent either a conversation turn OR a time-travel event.
type SimResult struct {
	Input    string
	Reply    string
	Duration time.Duration
	Error    error

	// Time-travel fields (mutually exclusive with Input/Reply).
	// If TimeTravelAdvance is non-empty, this is a time-travel event, not a turn.
	TimeTravelAdvance string   // e.g., "2h", "1d" — duration advanced
	SchedulesFired    []string // descriptions of schedules that fired

	// Per-turn metrics from bus events.
	Cost      float64
	ToolCalls int

	// Agent verdicts captured from bus events.
	MoodVerdict        string   // "auto_logged", "dropped_dedup", etc.
	MoodLabels         []string // emotion labels
	MoodValence        int
	MemoriesSaved      []string // memory tool call results (save_memory, update_memory)
	IntrospectionSaved []string // introspection tool call results (save_self_memory)
	FollowUpReply      string   // direct_message from notify_agent (memory → driver follow-up)
	ToolLog            []string // condensed log of all tool calls for the report

	// Cache + provider metrics accumulated across all LLM calls in this turn.
	CacheReadTokens  int            // total prompt tokens served from cache
	CacheWriteTokens int            // total prompt tokens written to cache
	Providers        map[string]int // provider name → call count for this turn
}

// simAdapter implements the Adapter interface for simulation runs.
// It feeds pre-loaded messages through the gateway pipeline one at a
// time, collecting responses. Subscribes to the tui.Bus to capture
// rich per-turn data (costs, mood, memories, introspection) alongside
// the reply text.
type simAdapter struct {
	cfg      config.AdapterConfig
	messages []SimMessage
	triggers SimTriggers
	options  SimOptions
	bus      *tui.Bus

	// store, when non-nil, is queried for each turn's authoritative cost
	// (SUM(cost_usd) FROM metrics WHERE message_id = ?) instead of trusting
	// TurnEndEvent.TotalCost — the same in-memory accumulator the TUI uses,
	// which only sums LLM completion costs and silently misses anything a
	// tool persists directly via SaveMetric (polaris_search, vision, STT).
	// nil in existing tests that don't need cost assertions.
	store memory.Store

	// convID is generated once at construction (not inside Start) so that
	// callers like cmd/sim_gw.go can read it via ConversationID() before
	// or during the run — needed so scheduler.Deps.GetConversationID can
	// route fired schedule messages into the same conversation the driver
	// is actually reading from.
	convID string

	msgCh    chan InboundMsg
	commands []CommandDef

	// compactHandler is set by the gateway after pipeline creation.
	compactHandler func(ctx context.Context, convID string) (string, error)

	// fireSchedulesFn force-fires all user-created schedules after advancing
	// the scheduler clock by the given duration. Set by cmd/sim_gw.go where
	// scheduler deps are available.
	fireSchedulesFn func(ctx context.Context, advanceDuration time.Duration) []string

	// workerResultCh receives worker completion data for follow-up turns.
	workerResultCh chan WorkerResult

	// Synchronous request/reply — same pattern as Gradio.
	pendingMu sync.Mutex
	pending   chan OutboundMsg

	// Results collected after the run completes.
	mu      sync.Mutex
	results []SimResult

	// Bus event capture — accumulates per-turn data from the event stream.
	captureMu    sync.Mutex
	activeTurn   *turnCapture
	finishedTurn chan *turnCapture // signals when TurnEndEvent finalizes a turn

	// Done is closed when all messages have been processed.
	Done chan struct{}
}

// SetFireSchedulesFn sets the callback for force-firing user schedules.
// The callback receives a duration to advance the scheduler clock before firing.
// Called from cmd/sim_gw.go where scheduler deps are available.
func (a *simAdapter) SetFireSchedulesFn(fn func(ctx context.Context, advanceDuration time.Duration) []string) {
	a.fireSchedulesFn = fn
}

// turnCapture accumulates bus events for a single turn.
type turnCapture struct {
	turnID    int64
	cost      float64
	toolCalls int
	toolLog   []string

	moodVerdict string
	moodLabels  []string
	moodValence int

	memoriesSaved      []string
	introspectionSaved []string
	followUpReply      string

	// Cache + provider metrics accumulated across all LLM calls in this turn.
	cacheReadTokens  int
	cacheWriteTokens int
	providers        map[string]int // provider name → call count
}

func newSimAdapter(acfg config.AdapterConfig, messages []SimMessage, triggers SimTriggers, opts SimOptions, bus *tui.Bus, workerResultCh chan WorkerResult, store memory.Store) (Adapter, error) {
	return &simAdapter{
		cfg:            acfg,
		messages:       messages,
		triggers:       triggers,
		options:        opts,
		bus:            bus,
		store:          store,
		convID:         fmt.Sprintf("sim-%d", time.Now().UnixMilli()),
		msgCh:          make(chan InboundMsg, 1),
		finishedTurn:   make(chan *turnCapture, 1),
		Done:           make(chan struct{}),
		workerResultCh: workerResultCh,
	}, nil
}

func (a *simAdapter) Name() string { return a.cfg.Name }

// ConversationID returns this sim run's conversation ID. Exposed so
// cmd/sim_gw.go can wire scheduler.Deps.GetConversationID to it — without
// this, fired schedule messages would land in a conversation the driver
// never reads from (they'd be tagged with a synthetic tg_<chatID> ID that
// has nothing to do with the sim's actual conversation).
func (a *simAdapter) ConversationID() string { return a.convID }

func (a *simAdapter) Capabilities() CapSet {
	return CapSet{}
}

// Start drives the scenario — sends each message through the pipeline
// sequentially, waits for the reply, collects results. A background
// goroutine subscribes to the bus and captures per-turn data.
func (a *simAdapter) Start(ctx context.Context) error {
	// Start bus capture goroutine.
	if a.bus != nil {
		go a.captureBusEvents(ctx)
	}

	convID := a.convID

	for i, msg := range a.messages {
		if ctx.Err() != nil {
			break
		}

		// Time-travel directive: advance the sim clock and fire schedules.
		// This lets sims test scheduled tasks without waiting real time.
		if msg.AdvanceTime != "" {
			duration, err := time.ParseDuration(msg.AdvanceTime)
			if err != nil {
				log.Errorf("sim: [%d/%d] invalid advance_time: %v", i+1, len(a.messages), err)
				continue
			}

			log.Infof("sim: [%d/%d] ⏰ TIME TRAVEL: advancing %s", i+1, len(a.messages), msg.AdvanceTime)

			// Fire all user schedules that are now due via the registered callback.
			// The callback advances the scheduler clock and then fires due schedules.
			var fired []string
			if a.fireSchedulesFn != nil {
				fired = a.fireSchedulesFn(ctx, duration)
				if len(fired) > 0 {
					log.Infof("sim: 🔔 fired %d schedule(s) after time travel", len(fired))
					for _, r := range fired {
						log.Info("sim: " + r)
					}
				} else {
					log.Info("sim: (no schedules were due)")
				}
			}

			// Append a time-travel result so it shows in the markdown report.
			a.mu.Lock()
			a.results = append(a.results, SimResult{
				TimeTravelAdvance: msg.AdvanceTime,
				SchedulesFired:    fired,
			})
			a.mu.Unlock()

			continue // don't process as a message — it's a time directive
		}

		start := time.Now()
		log.Infof("sim: [%d/%d] sending: %s", i+1, len(a.messages), truncateSimText(msg.Text, 80))

		inbound := InboundMsg{
			Text:           msg.Text,
			ConversationID: convID,
			AdapterName:    a.Name(),
			Timestamp:      time.Now(),
		}

		if msg.Image != "" {
			imgData, mime, err := loadImage(msg.Image)
			if err != nil {
				log.Errorf("sim: failed to load image %s: %v", msg.Image, err)
			} else {
				inbound.ImageBase64 = imgData
				inbound.ImageMIME = mime
			}
		}

		replyCh := make(chan OutboundMsg, 1)
		a.pendingMu.Lock()
		a.pending = replyCh
		a.pendingMu.Unlock()

		a.msgCh <- inbound

		var result SimResult
		result.Input = msg.Text

		select {
		case reply := <-replyCh:
			result.Reply = reply.Text
			result.Duration = time.Since(start)
		case <-ctx.Done():
			result.Error = ctx.Err()
		case <-time.After(simTurnTimeout):
			result.Error = fmt.Errorf("timeout after %s", simTurnTimeout)
		}

		// Wait for bus capture to finalize this turn's data.
		if a.bus != nil && result.Error == nil {
			result = a.enrichFromCapture(result)
		}

		a.mu.Lock()
		a.results = append(a.results, result)
		a.mu.Unlock()

		if result.Error != nil {
			log.Errorf("sim: [%d/%d] error: %v", i+1, len(a.messages), result.Error)
		} else {
			log.Infof("sim: [%d/%d] reply (%s, $%.4f, %d tools): %s",
				i+1, len(a.messages),
				result.Duration.Round(time.Millisecond),
				result.Cost, result.ToolCalls,
				truncateSimText(result.Reply, 100))
		}

		// Fire lifecycle triggers after this turn.
		turnNum := i + 1
		a.fireTriggers(ctx, turnNum, convID)

		// Check if the worker agent produced a result during this turn.
		// If so, inject a follow-up system turn so the driver can comment
		// on the finished report — same as EventWorkerComplete in production.
		if a.workerResultCh != nil {
			select {
			case wr := <-a.workerResultCh:
				log.Infof("sim: worker completed — injecting follow-up turn for %s", wr.TaskName)

				followUpStart := time.Now()
				systemPrompt := fmt.Sprintf(
					"[system] Your worker agent just finished a %s report.\n\n"+
						"Summary: %s\n\n"+
						"Share this with the user naturally — comment on what's interesting, "+
						"add your perspective. The report link will be attached automatically. "+
						"Keep it conversational, not like a system notification.",
					wr.TaskName, wr.Summary,
				)

				followUp := InboundMsg{
					Text:           systemPrompt,
					ConversationID: convID,
					AdapterName:    a.Name(),
					Timestamp:      time.Now(),
				}

				followUpReplyCh := make(chan OutboundMsg, 1)
				a.pendingMu.Lock()
				a.pending = followUpReplyCh
				a.pendingMu.Unlock()

				a.msgCh <- followUp

				var followUpResult SimResult
				followUpResult.Input = fmt.Sprintf("[worker:%s complete]", wr.TaskName)
				select {
				case reply := <-followUpReplyCh:
					followUpResult.Reply = reply.Text
					followUpResult.Duration = time.Since(followUpStart)
				case <-ctx.Done():
					followUpResult.Error = ctx.Err()
				case <-time.After(2 * time.Minute):
					followUpResult.Error = fmt.Errorf("worker follow-up timeout")
				}

				if a.bus != nil && followUpResult.Error == nil {
					followUpResult = a.enrichFromCapture(followUpResult)
				}

				a.mu.Lock()
				a.results = append(a.results, followUpResult)
				a.mu.Unlock()

				if followUpResult.Error != nil {
					log.Errorf("sim: worker follow-up error: %v", followUpResult.Error)
				} else {
					log.Infof("sim: worker follow-up (%s, $%.4f): %s",
						followUpResult.Duration.Round(time.Millisecond),
						followUpResult.Cost,
						truncateSimText(followUpResult.Reply, 100))
				}
			default:
				// No worker result — continue normally.
			}
		}

		// Delay between turns to avoid rate limits on free-tier models.
		if a.options.DelaySeconds > 0 && i < len(a.messages)-1 {
			time.Sleep(time.Duration(a.options.DelaySeconds) * time.Second)
		}
	}

	// Post-run: force-fire all user-created schedules (without time-travel).
	if a.triggers.FireSchedules && a.fireSchedulesFn != nil {
		results := a.fireSchedulesFn(ctx, 0) // 0 duration = no time advance
		for _, r := range results {
			log.Info("sim: fire-schedule result: " + r)
		}
	}

	// Post-run dream cycle.
	if a.triggers.RunDream {
		a.fireCommand(ctx, "dream", "")
	}

	// Post-run mood rollup — mirrors what the scheduler does at 21:00.
	if a.triggers.RunRollup {
		a.fireCommand(ctx, "rollup", "")
	}

	close(a.Done)
	return nil
}

// enrichFromCapture waits for the bus capture goroutine to finalize
// the turn data and merges it into the SimResult.
func (a *simAdapter) enrichFromCapture(result SimResult) SimResult {
	applyCapture := func(tc *turnCapture) {
		result.Cost = tc.cost
		result.ToolCalls = tc.toolCalls
		result.ToolLog = tc.toolLog
		result.MoodVerdict = tc.moodVerdict
		result.MoodLabels = tc.moodLabels
		result.MoodValence = tc.moodValence
		result.MemoriesSaved = tc.memoriesSaved
		result.IntrospectionSaved = tc.introspectionSaved
		result.FollowUpReply = tc.followUpReply
		result.CacheReadTokens = tc.cacheReadTokens
		result.CacheWriteTokens = tc.cacheWriteTokens
		result.Providers = tc.providers
	}

	select {
	case tc := <-a.finishedTurn:
		if tc != nil {
			applyCapture(tc)
		}
	case <-time.After(30 * time.Second):
		// Background agents (mood, introspection) may still be running.
		// Don't block forever — use whatever we have.
		a.captureMu.Lock()
		tc := a.activeTurn
		a.activeTurn = nil
		a.captureMu.Unlock()
		if tc != nil {
			applyCapture(tc)
		}
	}
	return result
}

// captureBusEvents subscribes to the tui.Bus and accumulates events
// into turnCapture structs. Each TurnStartEvent opens a new capture;
// TurnEndEvent finalizes it and signals the main loop.
func (a *simAdapter) captureBusEvents(ctx context.Context) {
	eventCh := a.bus.Subscribe(256)

	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-eventCh:
			if !ok {
				return
			}
			a.handleCaptureEvent(evt)
		}
	}
}

func (a *simAdapter) handleCaptureEvent(evt tui.Event) {
	switch e := evt.(type) {

	case tui.TurnStartEvent:
		a.captureMu.Lock()
		a.activeTurn = &turnCapture{turnID: e.TurnID}
		a.captureMu.Unlock()

	case tui.AgentIterEvent:
		a.captureMu.Lock()
		if tc := a.activeTurn; tc != nil {
			tc.cost += e.CostUSD
			tc.cacheReadTokens += e.CacheReadTokens
			tc.cacheWriteTokens += e.CacheWriteTokens
			if e.Provider != "" {
				if tc.providers == nil {
					tc.providers = make(map[string]int)
				}
				tc.providers[e.Provider]++
			}
		}
		a.captureMu.Unlock()

	case tui.ToolCallEvent:
		a.captureMu.Lock()
		tc := a.activeTurn
		if tc != nil {
			tc.toolCalls++

			// Build a condensed log line.
			icon := toolIcon(e.ToolName)
			if e.IsError {
				tc.toolLog = append(tc.toolLog, fmt.Sprintf("[%s] %s %s → ERROR: %s", e.Source, icon, e.ToolName, truncateSimText(e.Result, 60)))
			} else if e.ToolName == "think" {
				thought := extractThought(e.Args)
				tc.toolLog = append(tc.toolLog, fmt.Sprintf("[%s] %s %s", e.Source, icon, truncateSimText(thought, 80)))
			} else {
				tc.toolLog = append(tc.toolLog, fmt.Sprintf("[%s] %s %s → %s", e.Source, icon, e.ToolName, truncateSimText(e.Result, 60)))
			}

			// Capture memory saves by tool name + source.
			switch {
			case e.Source == "introspection" && (e.ToolName == "save_self_memory" || e.ToolName == "save_memory"):
				tc.introspectionSaved = append(tc.introspectionSaved, e.Result)
			case e.Source == "memory" && (e.ToolName == "save_memory" || e.ToolName == "update_memory"):
				tc.memoriesSaved = append(tc.memoriesSaved, e.Result)
			case e.ToolName == "notify_agent":
				tc.followUpReply = extractDirectMessage(e.Args)
			}
		}
		a.captureMu.Unlock()

	case tui.ReplyEvent:
		a.captureMu.Lock()
		if tc := a.activeTurn; tc != nil {
			tc.cost += e.CostUSD
			tc.cacheReadTokens += e.CacheReadTokens
			tc.cacheWriteTokens += e.CacheWriteTokens
			if e.Provider != "" {
				if tc.providers == nil {
					tc.providers = make(map[string]int)
				}
				tc.providers[e.Provider]++
			}
		}
		a.captureMu.Unlock()

	case tui.MoodEvent:
		a.captureMu.Lock()
		if tc := a.activeTurn; tc != nil {
			tc.moodVerdict = e.Action
			tc.moodLabels = e.Labels
			tc.moodValence = e.Valence
		}
		a.captureMu.Unlock()

	case tui.TurnEndEvent:
		a.captureMu.Lock()
		tc := a.activeTurn
		if tc != nil {
			// Query the DB rather than trusting e.TotalCost: that field is
			// summed purely from in-process LLM-call accounting and misses
			// any cost a tool persisted directly via ctx.Store.SaveMetric
			// (polaris_search, vision, STT) — CostForMessage sums the same
			// metrics table those calls write to, so it can't drift out of
			// sync as new cost sources get added. Falls back to e.TotalCost
			// if no store was wired in (e.g. existing tests).
			tc.cost = e.TotalCost
			if a.store != nil {
				if dbCost, err := a.store.CostForMessage(e.TurnID); err == nil {
					tc.cost = dbCost
				} else {
					log.Warn("sim: CostForMessage failed, falling back to in-memory total", "turn_id", e.TurnID, "err", err)
				}
			}
		}
		a.activeTurn = nil
		a.captureMu.Unlock()

		if tc != nil {
			// Non-blocking send — if the main loop isn't ready yet,
			// we don't want to block the capture goroutine.
			select {
			case a.finishedTurn <- tc:
			default:
			}
		}
	}
}

// fireTriggers checks if any lifecycle events should fire after this turn.
func (a *simAdapter) fireTriggers(ctx context.Context, turnNum int, convID string) {
	if a.triggers.CompactAfter > 0 && turnNum == a.triggers.CompactAfter {
		log.Infof("sim: triggering compaction after turn %d", turnNum)
		if a.compactHandler != nil {
			result, err := a.compactHandler(ctx, convID)
			if err != nil {
				log.Errorf("sim: compact failed: %v", err)
			} else {
				log.Infof("sim: /compact → %s", truncateSimText(result, 100))
			}
		} else {
			log.Warnf("sim: compact_after=%d but no compactHandler wired", turnNum)
		}
	}

	for _, dt := range a.triggers.DreamAfter {
		if turnNum == dt {
			log.Infof("sim: triggering dream cycle after turn %d", turnNum)
			a.fireCommand(ctx, "dream", "")
			break
		}
	}
}

// fireCommand executes a registered command by name.
func (a *simAdapter) fireCommand(ctx context.Context, name, args string) {
	for _, cmd := range a.commands {
		if cmd.Name == name {
			result, err := cmd.Handler(ctx, args)
			if err != nil {
				log.Errorf("sim: command /%s failed: %v", name, err)
			} else {
				log.Infof("sim: /%s → %s", name, truncateSimText(result, 100))
			}
			return
		}
	}
	log.Warnf("sim: command /%s not registered", name)
}

func (a *simAdapter) Stop() error { return nil }

func (a *simAdapter) Receive() <-chan InboundMsg { return a.msgCh }

func (a *simAdapter) Send(msg OutboundMsg) error {
	a.pendingMu.Lock()
	ch := a.pending
	a.pendingMu.Unlock()

	if ch != nil {
		ch <- msg
	}
	return nil
}

func (a *simAdapter) SendStatus(text string) error       { return nil }
func (a *simAdapter) StartTyping() func()                { return func() {} }
func (a *simAdapter) RegisterCommands(cmds []CommandDef) { a.commands = cmds }

// Results returns the collected sim results after Done is closed.
func (a *simAdapter) Results() []SimResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]SimResult{}, a.results...)
}

// --- Helpers ---

func loadImage(path string) (string, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}

	mime := http.DetectContentType(data)
	encoded := base64.StdEncoding.EncodeToString(data)
	return encoded, mime, nil
}

func truncateSimText(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func toolIcon(name string) string {
	switch name {
	case "think":
		return "🧠"
	case "reply":
		return "📝"
	case "done":
		return "✅"
	case "save_memory", "update_memory":
		return "💾"
	case "save_self_memory":
		return "🪡"
	case "remove_memory":
		return "🗑"
	case "recall_memories":
		return "🔍"
	case "web_search":
		return "🔍"
	case "web_read":
		return "🌐"
	case "no_action":
		return "⏭"
	case "use_tools":
		return "🧰"
	case "log_mood":
		return "💭"
	default:
		return "🔧"
	}
}

func extractThought(args string) string {
	// Args look like: {"thought":"User is feeling restless..."}
	if idx := strings.Index(args, `"thought":"`); idx >= 0 {
		start := idx + len(`"thought":"`)
		if end := strings.LastIndex(args[start:], `"`); end >= 0 {
			return args[start : start+end]
		}
	}
	return args
}

func extractDirectMessage(args string) string {
	// Args look like: {"summary":"...", "direct_message":"Hey, just a heads up..."}
	if idx := strings.Index(args, `"direct_message":"`); idx >= 0 {
		start := idx + len(`"direct_message":"`)
		if end := strings.LastIndex(args[start:], `"`); end >= 0 {
			return args[start : start+end]
		}
	}
	return ""
}

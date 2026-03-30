package cmd

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"her/agent"
	"her/config"
	"her/embed"
	"her/llm"
	"her/memory"
	"her/scrub"
	"her/search"
	"her/skills/loader"

	// Underscore import: registers the SQLite driver with database/sql.
	// We need this for the sim.db connection (separate from memory.Store
	// which handles its own driver registration via sqlite-vec).
	_ "github.com/mattn/go-sqlite3"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// suiteFlag holds the path to the suite YAML file, set via --suite / -s.
var suiteFlag string

// limitFlag caps the number of messages to send. 0 = all messages.
// Useful for testing with `--limit 1` to just send the first message
// without burning through all your tokens.
var limitFlag int

// delayFlag is the pause between turns in seconds. Defaults to 5s.
// In real usage, messages are minutes apart so rate limits aren't an issue.
// In sim mode we fire back-to-back, which can hit free-tier rate limits
// on the agent model. A few seconds between turns fixes this.
var delayFlag int

// agentModelFlag overrides the agent model from config.yaml for this run.
// Useful for comparing different models without editing the config file.
// Example: --agent-model "deepseek/deepseek-v3.2"
var agentModelFlag string

// simCmd defines the "her sim" subcommand. Cobra commands are just structs
// with metadata + a RunE function. RunE returns an error (vs Run which doesn't),
// so Cobra can print it nicely and set the exit code. Same idea as argparse
// subcommands in Python, but the wiring is struct-based instead of method calls.
var simCmd = &cobra.Command{
	Use:   "sim",
	Short: "Run a scripted conversation simulation",
	Long: `Runs a suite of scripted messages through the real chatbot pipeline
in a clean-room environment. Results are saved to sims/sim.db and a
Markdown report is generated in sims/results/.

Example:
  her sim --suite sims/getting-to-know-you.yaml`,
	RunE: runSim,
}

// init registers the sim command with the root command. In Go, init() functions
// run automatically when the package loads — like Python's module-level code,
// but guaranteed to run before main(). Each file can have its own init().
func init() {
	simCmd.Flags().StringVarP(&suiteFlag, "suite", "s", "", "path to suite YAML file (required)")
	simCmd.Flags().IntVarP(&limitFlag, "limit", "n", 0, "max messages to send (0 = all)")
	simCmd.Flags().IntVarP(&delayFlag, "delay", "d", 1, "seconds to wait between turns")
	simCmd.Flags().StringVar(&agentModelFlag, "agent-model", "", "override agent model for this run (e.g., deepseek/deepseek-v3.2)")
	// MarkFlagRequired makes Cobra error out if --suite is missing,
	// so we don't have to check it ourselves in runSim.
	simCmd.MarkFlagRequired("suite")
	rootCmd.AddCommand(simCmd)
}

// --------------------------------------------------------------------------
// Suite YAML structure
// --------------------------------------------------------------------------

// suite represents the YAML file that defines a scripted conversation.
// The struct tags tell the YAML parser which keys to look for.
type suite struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Tags        []string `yaml:"tags"`
	Messages    []string `yaml:"messages"`
}

// simTurnResult captures the outcome of one conversation turn during a
// simulation. Defined at package level (not inside a function) so it can
// be shared between runSim and generateReport. In Go, types defined
// inside a function are scoped to that function — they can't be used as
// parameters elsewhere.
type simTurnResult struct {
	userMsg  string
	botReply string
	elapsed  time.Duration
}

// --------------------------------------------------------------------------
// sim.db schema — separate from the production her.db
// --------------------------------------------------------------------------

// simDBSchema contains the CREATE TABLE statements for the simulation
// results database. This is a different database from her.db — it stores
// results across many sim runs so you can compare them.
const simDBSchema = `
CREATE TABLE IF NOT EXISTS sim_runs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
	suite_name TEXT NOT NULL,
	suite_path TEXT NOT NULL,
	chat_model TEXT,
	agent_model TEXT,
	total_messages INTEGER,
	total_cost_usd REAL,
	duration_ms INTEGER
);

CREATE TABLE IF NOT EXISTS sim_messages (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id INTEGER NOT NULL REFERENCES sim_runs(id),
	turn_number INTEGER,
	timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
	role TEXT NOT NULL,
	content TEXT NOT NULL,
	conversation_id TEXT
);

CREATE TABLE IF NOT EXISTS sim_facts (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id INTEGER NOT NULL REFERENCES sim_runs(id),
	timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
	fact TEXT NOT NULL,
	category TEXT,
	subject TEXT DEFAULT 'user',
	importance INTEGER DEFAULT 5,
	active BOOLEAN DEFAULT 1
);

CREATE TABLE IF NOT EXISTS sim_mood_entries (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id INTEGER NOT NULL REFERENCES sim_runs(id),
	timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
	rating INTEGER NOT NULL,
	note TEXT,
	tags TEXT,
	source TEXT DEFAULT 'inferred'
);

CREATE TABLE IF NOT EXISTS sim_metrics (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id INTEGER NOT NULL REFERENCES sim_runs(id),
	timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
	model TEXT NOT NULL,
	prompt_tokens INTEGER,
	completion_tokens INTEGER,
	total_tokens INTEGER,
	cost_usd REAL,
	latency_ms INTEGER
);

CREATE TABLE IF NOT EXISTS sim_agent_turns (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id INTEGER NOT NULL REFERENCES sim_runs(id),
	turn_number INTEGER,
	timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
	turn_index INTEGER,
	role TEXT NOT NULL,
	tool_name TEXT,
	tool_args TEXT,
	content TEXT
);

CREATE TABLE IF NOT EXISTS sim_summaries (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id INTEGER NOT NULL REFERENCES sim_runs(id),
	timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
	conversation_id TEXT,
	summary TEXT NOT NULL,
	messages_summarized INTEGER
);
`

// --------------------------------------------------------------------------
// Main command logic
// --------------------------------------------------------------------------

// runSim is the entry point for "her sim --suite path/to/suite.yaml".
// It loads a suite, runs every message through the real agent pipeline
// using a throwaway database, then copies the results to a persistent
// sim.db for later comparison.
func runSim(cmd *cobra.Command, args []string) error {
	startTime := time.Now()

	// ------------------------------------------------------------------
	// 1. Parse the suite YAML
	// ------------------------------------------------------------------

	// os.ReadFile reads an entire file into a byte slice — like Python's
	// open(path).read(). In Go, files return []byte, not strings.
	suiteBytes, err := os.ReadFile(suiteFlag)
	if err != nil {
		return fmt.Errorf("reading suite file: %w", err)
	}

	var s suite
	// yaml.Unmarshal is like json.loads() in Python — it takes raw bytes
	// and fills in a struct. The &s passes a pointer so Unmarshal can
	// modify the struct in place.
	if err := yaml.Unmarshal(suiteBytes, &s); err != nil {
		return fmt.Errorf("parsing suite YAML: %w", err)
	}

	if len(s.Messages) == 0 {
		return fmt.Errorf("suite %q has no messages", s.Name)
	}

	log.Info("Simulation starting", "suite", s.Name, "messages", len(s.Messages))
	fmt.Printf("\n=== Simulation: %s ===\n", s.Name)
	if s.Description != "" {
		fmt.Printf("    %s\n", s.Description)
	}
	fmt.Printf("    %d messages to send\n\n", len(s.Messages))

	// ------------------------------------------------------------------
	// 2. Load config (skip Telegram + LLM key checks)
	// ------------------------------------------------------------------

	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// Export config secrets as process-level env vars so skills can
	// find them. Without this, skills like web_search fail because
	// TAVILY_API_KEY isn't in the environment. run.go does this too.
	cfg.ExportEnv()

	// Warn but don't fatal on missing API key — the user might just be
	// testing the sim harness itself.
	if cfg.LLM.APIKey == "" {
		log.Warn("LLM API key not set — agent calls will fail")
	}

	// --agent-model flag overrides the config value. This mutates cfg so
	// both the run logic and report generator see the same model name.
	if agentModelFlag != "" {
		log.Info("Agent model overridden via --agent-model", "model", agentModelFlag)
		cfg.Agent.Model = agentModelFlag
	}

	// ------------------------------------------------------------------
	// 3. Open/create sims/sim.db for persistent results
	// ------------------------------------------------------------------

	// os.MkdirAll is like Python's os.makedirs(exist_ok=True) — creates
	// the directory and all parents, no error if it already exists.
	if err := os.MkdirAll("sims/results", 0o755); err != nil {
		return fmt.Errorf("creating sims directory: %w", err)
	}

	simDB, err := sql.Open("sqlite3", "sims/sim.db?_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		return fmt.Errorf("opening sim.db: %w", err)
	}
	defer simDB.Close()

	// Execute all CREATE TABLE statements. We split on semicolons and
	// run each one individually because sql.Exec only runs one statement
	// per call in the go-sqlite3 driver.
	for _, stmt := range strings.Split(simDBSchema, ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := simDB.Exec(stmt); err != nil {
			return fmt.Errorf("initializing sim.db schema: %w", err)
		}
	}

	// Insert a new run row. We'll update it with totals at the end.
	agentModel := cfg.Agent.Model
	if agentModel == "" {
		agentModel = "liquid/lfm-2.5-1.2b-instruct:free"
	}

	res, err := simDB.Exec(
		`INSERT INTO sim_runs (suite_name, suite_path, chat_model, agent_model, total_messages)
		 VALUES (?, ?, ?, ?, ?)`,
		s.Name, suiteFlag, cfg.LLM.Model, agentModel, len(s.Messages),
	)
	if err != nil {
		return fmt.Errorf("inserting sim run: %w", err)
	}
	runID, _ := res.LastInsertId()
	conversationID := fmt.Sprintf("sim-run-%d", runID)

	log.Info("Sim run created", "run_id", runID, "conversation_id", conversationID)

	// ------------------------------------------------------------------
	// 4. Create temp DB for the pipeline (clean-room)
	// ------------------------------------------------------------------

	// os.CreateTemp creates a file in the OS temp directory with a unique
	// name. The "*" in the pattern gets replaced with a random string.
	// This is like Python's tempfile.NamedTemporaryFile(delete=False).
	tmpFile, err := os.CreateTemp("", "her-sim-*.db")
	if err != nil {
		return fmt.Errorf("creating temp DB file: %w", err)
	}
	tmpDBPath := tmpFile.Name()
	tmpFile.Close() // Close immediately — NewStore will reopen it.

	// Clean up the temp DB when we're done. defer runs when the function
	// returns — like a Python context manager's __exit__, but for any
	// cleanup action. Multiple defers run in LIFO order (last in, first out).
	defer os.Remove(tmpDBPath)

	store, err := memory.NewStore(tmpDBPath, cfg.Embed.Dimension)
	if err != nil {
		return fmt.Errorf("creating temp store: %w", err)
	}
	defer store.Close()

	// ------------------------------------------------------------------
	// 5. Create LLM + embed + search clients (same pattern as run.go)
	// ------------------------------------------------------------------

	chatClient := llm.NewClient(
		cfg.LLM.BaseURL,
		cfg.LLM.APIKey,
		cfg.LLM.Model,
		cfg.LLM.Temperature,
		cfg.LLM.MaxTokens,
	)
	if cfg.LLM.Fallback != nil {
		chatClient.WithFallback(cfg.LLM.Fallback.Model, cfg.LLM.Fallback.Temperature, cfg.LLM.Fallback.MaxTokens)
	}

	agentTemp := cfg.Agent.Temperature
	if agentTemp == 0 {
		agentTemp = 0.1
	}
	agentMaxTokens := cfg.Agent.MaxTokens
	if agentMaxTokens == 0 {
		agentMaxTokens = 512
	}
	agentClient := llm.NewClient(
		cfg.LLM.BaseURL,
		cfg.LLM.APIKey,
		agentModel,
		agentTemp,
		agentMaxTokens,
	)
	if cfg.Agent.Fallback != nil {
		agentClient.WithFallback(cfg.Agent.Fallback.Model, cfg.Agent.Fallback.Temperature, cfg.Agent.Fallback.MaxTokens)
	}

	// --- Classifier client (optional) ---
	// Enable the classifier in sims so we can test rejection behavior.
	var classifierClient *llm.Client
	if cfg.Classifier.Model != "" {
		classifierMaxTokens := cfg.Classifier.MaxTokens
		if classifierMaxTokens == 0 {
			classifierMaxTokens = 64
		}
		classifierClient = llm.NewClient(cfg.LLM.BaseURL, cfg.LLM.APIKey, cfg.Classifier.Model, cfg.Classifier.Temperature, classifierMaxTokens)
		log.Info("classifier enabled for sim", "model", cfg.Classifier.Model)
	}

	var embedClient *embed.Client
	if cfg.Embed.BaseURL != "" && cfg.Embed.Model != "" {
		embedClient = embed.NewClient(cfg.Embed.BaseURL, cfg.Embed.Model, cfg.Embed.Dimension)
	}

	var tavilyClient *search.TavilyClient
	if cfg.Search.TavilyAPIKey != "" {
		tavilyClient = search.NewTavilyClient(cfg.Search.TavilyAPIKey, cfg.Search.TavilyBaseURL)
	}

	// Load skills registry for find_skill/run_skill.
	skillsDir := filepath.Join(filepath.Dir(cfgFile), "skills")
	skillReg := loader.NewRegistry(skillsDir, embedClient)
	if count, err := skillReg.Load(); err != nil {
		log.Warn("failed to load skills", "err", err)
	} else if count > 0 {
		log.Info("skills loaded", "count", count)
	}
	if embedClient != nil {
		loader.SetEmbedClient(embedClient)
	}

	// Start DB proxy so skills with database permissions (like log_mood)
	// can write to the sim's temp DB. Same as cmd/run.go but pointing at
	// tmpDBPath instead of cfg.Memory.DBPath — skills write to the
	// clean-room DB, not the real her.db.
	dbProxy, dbProxyErr := loader.NewDBProxy(tmpDBPath, nil)
	if dbProxyErr != nil {
		log.Warn("db proxy failed to start — skills will not have database access", "err", dbProxyErr)
	} else {
		loader.SetDBProxy(dbProxy)
		defer dbProxy.Close()
		log.Info("db proxy started for sim", "port", dbProxy.Port())
	}

	// ------------------------------------------------------------------
	// 6. Override persona file to a temp empty file
	// ------------------------------------------------------------------

	// The persona file normally accumulates across conversations. For
	// simulations we want a blank slate, so we create an empty temp file.
	tmpPersona, err := os.CreateTemp("", "her-sim-persona-*.md")
	if err != nil {
		return fmt.Errorf("creating temp persona file: %w", err)
	}
	tmpPersonaPath := tmpPersona.Name()
	tmpPersona.Close()
	defer os.Remove(tmpPersonaPath)

	cfg.Persona.PersonaFile = tmpPersonaPath

	// ------------------------------------------------------------------
	// 7. Message loop — send each message through the real pipeline
	// ------------------------------------------------------------------

	// turnResults collects the bot's reply for each turn so we can build
	// the report afterward. make() pre-allocates the slice with capacity
	// for all messages — like Python's [None] * n but for a typed slice.
	// Apply --limit flag: if set, only send the first N messages.
	// This lets you test with `her sim --suite sims/intro.yaml -n 1`
	// to just run one message without burning through all your tokens.
	messages := s.Messages
	if limitFlag > 0 && limitFlag < len(messages) {
		messages = messages[:limitFlag]
		fmt.Printf("    (limited to first %d messages via --limit)\n\n", limitFlag)
	}

	turnResults := make([]simTurnResult, 0, len(messages))

	total := len(messages)
	for i, msg := range messages {
		turnStart := time.Now()

		fmt.Printf("[%d/%d] %s: %s\n", i+1, total, cfg.Identity.User, msg)

		// Save the user message to the temp store.
		msgID, err := store.SaveMessage("user", msg, "", conversationID)
		if err != nil {
			log.Error("failed to save message", "err", err)
			continue
		}

		// Scrub PII from the message, just like the real pipeline does.
		scrubResult := scrub.Scrub(msg)
		if err := store.UpdateMessageScrubbed(msgID, scrubResult.Text); err != nil {
			log.Error("failed to update scrubbed content", "err", err)
		}

		// StatusCallback updates the user on what the agent is doing.
		// In production this edits the Telegram message; here we just
		// print to stdout so you can watch the agent think.
		statusCallback := func(status string) error {
			fmt.Printf("       [status] %s\n", status)
			return nil
		}

		// TraceCallback surfaces agent internals (compaction, persona
		// reflection, etc.) in the sim output. In production this edits
		// a Telegram message; here we log to stdout so it appears in
		// the sim trace alongside tool calls and replies.
		traceCallback := func(html string) error {
			fmt.Printf("       [trace] %s\n", html)
			return nil
		}

		// Run the full agent pipeline — same call the Telegram bot makes.
		result, err := agent.Run(agent.RunParams{
			AgentLLM:            agentClient,
			ChatLLM:             chatClient,
			VisionLLM:           nil, // no image support in sim
			ClassifierLLM:       classifierClient, // nil if not configured, active if classifier section in config
			Store:               store,
			EmbedClient:         embedClient,
			SimilarityThreshold: cfg.Embed.SimilarityThreshold,
			TavilyClient:        tavilyClient,
			Cfg:                 cfg,
			ScrubbedUserMessage: scrubResult.Text,
			ScrubVault:          scrubResult.Vault,
			ConversationID:      conversationID,
			TriggerMsgID:        msgID,
			StatusCallback:      statusCallback,
			TraceCallback:       traceCallback,
			TTSCallback:         nil, // no TTS in sim
			ReflectionThreshold: cfg.Persona.ReflectionMemoryThreshold,
			RewriteEveryN:       cfg.Persona.RewriteEveryNReflections,
			ConfigPath:          cfgFile,
			SkillRegistry:       skillReg,
		})
		if err != nil {
			log.Error("agent.Run failed", "turn", i+1, "err", err)
			fmt.Printf("       %s: [ERROR: %s]\n\n", cfg.Identity.Her, err)
			turnResults = append(turnResults, simTurnResult{
				userMsg:  msg,
				botReply: fmt.Sprintf("[ERROR: %s]", err),
				elapsed:  time.Since(turnStart),
			})
			continue
		}

		elapsed := time.Since(turnStart)
		fmt.Printf("       %s: %s\n", cfg.Identity.Her, result.ReplyText)
		fmt.Printf("       (%s)\n\n", elapsed.Round(time.Millisecond))

		turnResults = append(turnResults, simTurnResult{
			userMsg:  msg,
			botReply: result.ReplyText,
			elapsed:  elapsed,
		})

		// Pause between turns to avoid hitting rate limits on free-tier
		// models. In real usage the user types slowly enough that this
		// isn't needed, but sim fires back-to-back.
		if delayFlag > 0 && i < total-1 {
			fmt.Printf("       (waiting %ds before next turn...)\n\n", delayFlag)
			time.Sleep(time.Duration(delayFlag) * time.Second)
		}
	}

	totalDuration := time.Since(startTime)

	// ------------------------------------------------------------------
	// 8. Copy data from temp DB to sim.db
	// ------------------------------------------------------------------

	// We open the temp DB a second time with raw sql.Open to query it
	// directly. The Store struct doesn't expose its internal *sql.DB,
	// and we need to run raw SELECT queries that don't map to any
	// existing Store method. This is fine — SQLite supports concurrent
	// readers via WAL mode.
	tmpDB, err := sql.Open("sqlite3", tmpDBPath+"?_journal_mode=WAL&mode=ro")
	if err != nil {
		return fmt.Errorf("reopening temp DB for copy: %w", err)
	}
	defer tmpDB.Close()

	// Copy messages
	if err := copyMessages(tmpDB, simDB, runID, conversationID); err != nil {
		log.Error("failed to copy messages", "err", err)
	}

	// Copy facts
	if err := copyFacts(tmpDB, simDB, runID); err != nil {
		log.Error("failed to copy facts", "err", err)
	}

	// Copy mood entries
	if err := copyMoodEntries(tmpDB, simDB, runID); err != nil {
		log.Error("failed to copy mood entries", "err", err)
	}

	// Copy metrics and calculate total cost
	totalCost, err := copyMetrics(tmpDB, simDB, runID)
	if err != nil {
		log.Error("failed to copy metrics", "err", err)
	}

	// Copy agent turns
	if err := copyAgentTurns(tmpDB, simDB, runID, total); err != nil {
		log.Error("failed to copy agent turns", "err", err)
	}

	// Copy compaction summaries — these show when conversation history
	// exceeded the token budget and older messages were compressed into
	// a summary. Without this, compaction is invisible in sim results.
	if err := copySummaries(tmpDB, simDB, runID); err != nil {
		log.Error("failed to copy summaries", "err", err)
	}

	// Update the run row with final totals.
	_, err = simDB.Exec(
		`UPDATE sim_runs SET total_cost_usd = ?, duration_ms = ? WHERE id = ?`,
		totalCost, totalDuration.Milliseconds(), runID,
	)
	if err != nil {
		log.Error("failed to update sim run totals", "err", err)
	}

	// ------------------------------------------------------------------
	// 9. Generate markdown report
	// ------------------------------------------------------------------

	report, err := generateReport(simDB, runID, &s, cfg, turnResults, totalCost, totalDuration)
	if err != nil {
		log.Error("failed to generate report", "err", err)
	} else {
		// Sanitize the suite name for use as a filename. Replace spaces
		// and special characters with hyphens.
		safeName := strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
				return r
			}
			return '-'
		}, s.Name)
		safeName = strings.ToLower(safeName)

		reportPath := filepath.Join("sims", "results", fmt.Sprintf("%s-run%d.md", safeName, runID))
		if err := os.WriteFile(reportPath, []byte(report), 0o644); err != nil {
			log.Error("failed to write report", "err", err)
		} else {
			fmt.Printf("Report saved: %s\n", reportPath)
		}
	}

	// ------------------------------------------------------------------
	// 10. Print summary
	// ------------------------------------------------------------------

	fmt.Printf("\n=== Simulation Complete ===\n")
	fmt.Printf("    Suite:    %s\n", s.Name)
	fmt.Printf("    Run ID:   %d\n", runID)
	fmt.Printf("    Messages: %d\n", total)
	fmt.Printf("    Duration: %s\n", totalDuration.Round(time.Millisecond))
	fmt.Printf("    Cost:     $%.4f\n", totalCost)
	fmt.Printf("    Results:  sims/sim.db\n\n")

	return nil
}

// --------------------------------------------------------------------------
// Data copy helpers — move rows from the temp pipeline DB into sim.db
// --------------------------------------------------------------------------

// copyMessages copies all messages from the temp DB into sim_messages,
// tagging each with the run_id. We query turn_number from row ordering
// since messages are inserted sequentially.
func copyMessages(tmpDB, simDB *sql.DB, runID int64, convID string) error {
	rows, err := tmpDB.Query(
		`SELECT id, timestamp, role, content_raw, conversation_id
		 FROM messages WHERE conversation_id = ?
		 ORDER BY id ASC`, convID,
	)
	if err != nil {
		return fmt.Errorf("querying messages: %w", err)
	}
	// defer rows.Close() is critical — without it, the database connection
	// stays locked. Same idea as closing a file handle in Python.
	defer rows.Close()

	turnNum := 0
	for rows.Next() {
		var id int64
		var ts, role, content, cid string
		if err := rows.Scan(&id, &ts, &role, &content, &cid); err != nil {
			return fmt.Errorf("scanning message: %w", err)
		}
		turnNum++
		_, err := simDB.Exec(
			`INSERT INTO sim_messages (run_id, turn_number, timestamp, role, content, conversation_id)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			runID, turnNum, ts, role, content, cid,
		)
		if err != nil {
			return fmt.Errorf("inserting sim_message: %w", err)
		}
	}
	// rows.Err() catches errors that happened during iteration — Next()
	// can silently fail, so this final check is a Go database idiom.
	return rows.Err()
}

// copyFacts copies all facts from the temp DB into sim_facts.
func copyFacts(tmpDB, simDB *sql.DB, runID int64) error {
	rows, err := tmpDB.Query(
		`SELECT timestamp, fact, category, COALESCE(subject, 'user'), importance, active
		 FROM facts ORDER BY id ASC`,
	)
	if err != nil {
		return fmt.Errorf("querying facts: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var ts, fact, category, subject string
		var importance int
		var active bool
		if err := rows.Scan(&ts, &fact, &category, &subject, &importance, &active); err != nil {
			return fmt.Errorf("scanning fact: %w", err)
		}
		_, err := simDB.Exec(
			`INSERT INTO sim_facts (run_id, timestamp, fact, category, subject, importance, active)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			runID, ts, fact, category, subject, importance, active,
		)
		if err != nil {
			return fmt.Errorf("inserting sim_fact: %w", err)
		}
	}
	return rows.Err()
}

// copySummaries copies compaction summaries from the temp DB into
// sim_summaries. Each row represents one compaction event where older
// messages were compressed into a running summary.
func copySummaries(tmpDB, simDB *sql.DB, runID int64) error {
	rows, err := tmpDB.Query(
		`SELECT timestamp, conversation_id, summary, messages_start_id, messages_end_id
		 FROM summaries ORDER BY id ASC`,
	)
	if err != nil {
		return fmt.Errorf("querying summaries: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var ts, convID, summary string
		var startID, endID int64
		if err := rows.Scan(&ts, &convID, &summary, &startID, &endID); err != nil {
			return fmt.Errorf("scanning summary: %w", err)
		}
		// messages_summarized = how many messages were compressed.
		// endID - startID is approximate but directionally useful.
		msgCount := endID - startID
		if msgCount < 0 {
			msgCount = 0
		}
		_, err := simDB.Exec(
			`INSERT INTO sim_summaries (run_id, timestamp, conversation_id, summary, messages_summarized)
			 VALUES (?, ?, ?, ?, ?)`,
			runID, ts, convID, summary, msgCount,
		)
		if err != nil {
			return fmt.Errorf("inserting sim_summary: %w", err)
		}
	}
	return rows.Err()
}

// copyMoodEntries copies mood entries from the temp DB into sim_mood_entries.
func copyMoodEntries(tmpDB, simDB *sql.DB, runID int64) error {
	rows, err := tmpDB.Query(
		`SELECT timestamp, rating, note, tags, source FROM mood_entries ORDER BY id ASC`,
	)
	if err != nil {
		return fmt.Errorf("querying mood entries: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var ts string
		var rating int
		// sql.NullString handles NULL values from the database. In Go,
		// a regular string can't be null — NullString has a .Valid bool
		// that tells you whether the value was NULL or not.
		var note, tags, source sql.NullString
		if err := rows.Scan(&ts, &rating, &note, &tags, &source); err != nil {
			return fmt.Errorf("scanning mood entry: %w", err)
		}
		_, err := simDB.Exec(
			`INSERT INTO sim_mood_entries (run_id, timestamp, rating, note, tags, source)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			runID, ts, rating, note, tags, source,
		)
		if err != nil {
			return fmt.Errorf("inserting sim_mood_entry: %w", err)
		}
	}
	return rows.Err()
}

// copyMetrics copies metrics from the temp DB into sim_metrics and returns
// the total cost across all LLM calls in this run.
func copyMetrics(tmpDB, simDB *sql.DB, runID int64) (float64, error) {
	rows, err := tmpDB.Query(
		`SELECT timestamp, model, prompt_tokens, completion_tokens, total_tokens, cost_usd, latency_ms
		 FROM metrics ORDER BY id ASC`,
	)
	if err != nil {
		return 0, fmt.Errorf("querying metrics: %w", err)
	}
	defer rows.Close()

	var totalCost float64
	for rows.Next() {
		var ts, model string
		var promptTok, completionTok, totalTok, latencyMs int
		var costUSD float64
		if err := rows.Scan(&ts, &model, &promptTok, &completionTok, &totalTok, &costUSD, &latencyMs); err != nil {
			return totalCost, fmt.Errorf("scanning metric: %w", err)
		}
		totalCost += costUSD
		_, err := simDB.Exec(
			`INSERT INTO sim_metrics (run_id, timestamp, model, prompt_tokens, completion_tokens, total_tokens, cost_usd, latency_ms)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			runID, ts, model, promptTok, completionTok, totalTok, costUSD, latencyMs,
		)
		if err != nil {
			return totalCost, fmt.Errorf("inserting sim_metric: %w", err)
		}
	}
	return totalCost, rows.Err()
}

// copyAgentTurns copies agent trace data from the temp DB into sim_agent_turns.
// We derive turn_number from message_id ordering — each unique message_id
// represents one conversation turn.
func copyAgentTurns(tmpDB, simDB *sql.DB, runID int64, totalTurns int) error {
	rows, err := tmpDB.Query(
		`SELECT timestamp, message_id, turn_index, role, tool_name, tool_args, content
		 FROM agent_turns ORDER BY id ASC`,
	)
	if err != nil {
		return fmt.Errorf("querying agent turns: %w", err)
	}
	defer rows.Close()

	// Track message_id → turn_number mapping so we can group agent steps
	// by the conversation turn they belong to.
	msgToTurn := make(map[int64]int)
	turnCounter := 0

	for rows.Next() {
		var ts string
		var msgID sql.NullInt64
		var turnIndex int
		var role string
		var toolName, toolArgs, content sql.NullString
		if err := rows.Scan(&ts, &msgID, &turnIndex, &role, &toolName, &toolArgs, &content); err != nil {
			return fmt.Errorf("scanning agent turn: %w", err)
		}

		// Map message_id to a sequential turn number.
		turnNum := 0
		if msgID.Valid {
			if _, exists := msgToTurn[msgID.Int64]; !exists {
				turnCounter++
				msgToTurn[msgID.Int64] = turnCounter
			}
			turnNum = msgToTurn[msgID.Int64]
		}

		_, err := simDB.Exec(
			`INSERT INTO sim_agent_turns (run_id, turn_number, timestamp, turn_index, role, tool_name, tool_args, content)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			runID, turnNum, ts, turnIndex, role, toolName, toolArgs, content,
		)
		if err != nil {
			return fmt.Errorf("inserting sim_agent_turn: %w", err)
		}
	}
	return rows.Err()
}

// --------------------------------------------------------------------------
// Report generation
// --------------------------------------------------------------------------

// generateReport builds a Markdown report summarizing the simulation run.
// It pulls data from both the sim.db (facts, metrics) and the turn results
// collected during the message loop.
func generateReport(
	simDB *sql.DB,
	runID int64,
	s *suite,
	cfg *config.Config,
	turns []simTurnResult,
	totalCost float64,
	totalDuration time.Duration,
) (string, error) {
	// strings.Builder is Go's equivalent of Python's io.StringIO or
	// just building a string with a list of parts and joining them.
	// It's more efficient than repeated string concatenation because
	// strings in Go are immutable (each += creates a new string).
	var b strings.Builder

	agentModel := cfg.Agent.Model
	if agentModel == "" {
		agentModel = "liquid/lfm-2.5-1.2b-instruct:free"
	}

	// Header
	fmt.Fprintf(&b, "# Simulation Report: %s\n\n", s.Name)
	fmt.Fprintf(&b, "**Run:** #%d | **Date:** %s | **Chat model:** %s | **Agent model:** %s | **Cost:** $%.4f\n\n",
		runID,
		time.Now().Format("2006-01-02 15:04"), // Go's time format uses a reference date, not %Y-%m-%d
		cfg.LLM.Model,
		agentModel,
		totalCost,
	)

	// Conversation section
	b.WriteString("## Conversation\n\n")
	for i, turn := range turns {
		fmt.Fprintf(&b, "### Turn %d\n", i+1)
		fmt.Fprintf(&b, "**%s:** %s\n\n", cfg.Identity.User, turn.userMsg)
		fmt.Fprintf(&b, "**%s:** %s\n\n", cfg.Identity.Her, turn.botReply)

		// Add agent trace as a collapsible details block.
		writeAgentTrace(&b, simDB, runID, i+1)

		b.WriteString("---\n\n")
	}

	// Facts section
	writeFactsSection(&b, simDB, runID)

	// Mood section
	writeMoodSection(&b, simDB, runID)

	// Compaction summaries section
	writeSummariesSection(&b, simDB, runID)

	// Cost summary
	writeCostSection(&b, simDB, runID)

	return b.String(), nil
}

// writeAgentTrace writes a collapsible <details> block with the agent's
// tool calls for a specific turn number.
func writeAgentTrace(b *strings.Builder, simDB *sql.DB, runID int64, turnNum int) {
	rows, err := simDB.Query(
		`SELECT turn_index, role, tool_name, tool_args, content
		 FROM sim_agent_turns WHERE run_id = ? AND turn_number = ?
		 ORDER BY turn_index ASC`,
		runID, turnNum,
	)
	if err != nil {
		return
	}
	defer rows.Close()

	// Collect all rows first so we know the count for the summary line.
	type agentStep struct {
		turnIndex int
		role      string
		toolName  sql.NullString
		toolArgs  sql.NullString
		content   sql.NullString
	}
	var steps []agentStep

	for rows.Next() {
		var step agentStep
		if err := rows.Scan(&step.turnIndex, &step.role, &step.toolName, &step.toolArgs, &step.content); err != nil {
			continue
		}
		steps = append(steps, step)
	}

	if len(steps) == 0 {
		// No agent trace = the agent model likely failed (rate limit,
		// error, etc.) and the fallback reply kicked in. Flag it loudly.
		b.WriteString("> **⚠ NO AGENT TRACE** — the agent model produced no tool calls for this turn.\n")
		b.WriteString("> The reply above was generated by the fallback path (direct chat model call).\n")
		b.WriteString("> This usually means the agent model was rate-limited or returned an error.\n\n")
		return
	}

	// Count just the tool calls (assistant role) for the summary line.
	var callCount int
	for _, step := range steps {
		if step.role == "assistant" && step.toolName.Valid {
			callCount++
		}
	}

	// Check if the agent completed its job — a healthy turn always has
	// at least a reply + done. If those are missing, something went wrong.
	var hasReply, hasDone bool
	for _, step := range steps {
		if step.role == "assistant" && step.toolName.Valid {
			switch step.toolName.String {
			case "reply":
				hasReply = true
			case "done":
				hasDone = true
			}
		}
	}

	if !hasReply {
		b.WriteString("> **⚠ INCOMPLETE TURN** — the agent never called `reply`. The response above came from the fallback path.\n\n")
	} else if !hasDone {
		b.WriteString("> **⚠ INCOMPLETE TURN** — the agent called `reply` but never called `done` (loop may have been cut short).\n\n")
	}

	fmt.Fprintf(b, "<details><summary>Agent trace (%d tool calls)</summary>\n\n", callCount)

	// Render each step as a call → result pair. The agent_turns table
	// alternates: assistant (the tool call) then tool (the result).
	// We show both so you can see what the agent decided AND what happened.
	for _, step := range steps {
		if step.role == "assistant" && step.toolName.Valid {
			// This is the agent deciding to call a tool.
			toolName := step.toolName.String
			args := step.toolArgs.String
			if args == "" || args == "{}" {
				fmt.Fprintf(b, "**→ `%s`**\n\n", toolName)
			} else {
				// Pretty-print the args. Don't truncate — the whole
				// point of the report is to see everything.
				fmt.Fprintf(b, "**→ `%s`**\n```json\n%s\n```\n\n", toolName, args)
			}
		} else if step.role == "tool" {
			// This is the tool's response — what actually happened.
			content := step.content.String
			toolName := ""
			if step.toolName.Valid {
				toolName = step.toolName.String
			}
			if content == "" {
				content = "(empty response)"
			}
			// Show the result in a blockquote so it's visually distinct
			// from the call. Indent each line with >.
			lines := strings.Split(content, "\n")
			for _, line := range lines {
				fmt.Fprintf(b, "> %s\n", line)
			}
			if toolName != "" {
				fmt.Fprintf(b, "> *— %s result*\n", toolName)
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("</details>\n\n")
}

// writeFactsSection writes the facts table to the report.
func writeFactsSection(b *strings.Builder, simDB *sql.DB, runID int64) {
	rows, err := simDB.Query(
		`SELECT id, fact, category, subject, importance
		 FROM sim_facts WHERE run_id = ? ORDER BY id ASC`, runID,
	)
	if err != nil {
		return
	}
	defer rows.Close()

	type factRow struct {
		id         int64
		fact       string
		category   sql.NullString
		subject    string
		importance int
	}
	var facts []factRow
	for rows.Next() {
		var f factRow
		if err := rows.Scan(&f.id, &f.fact, &f.category, &f.subject, &f.importance); err != nil {
			continue
		}
		facts = append(facts, f)
	}

	fmt.Fprintf(b, "## Facts Saved (%d)\n\n", len(facts))
	if len(facts) > 0 {
		b.WriteString("| ID | Fact | Category | Subject | Importance |\n")
		b.WriteString("|----|------|----------|---------|------------|\n")
		for _, f := range facts {
			cat := ""
			if f.category.Valid {
				cat = f.category.String
			}
			fmt.Fprintf(b, "| %d | %s | %s | %s | %d |\n",
				f.id, f.fact, cat, f.subject, f.importance)
		}
	}
	b.WriteString("\n")
}

// writeMoodSection writes the mood entries table to the report.
func writeMoodSection(b *strings.Builder, simDB *sql.DB, runID int64) {
	rows, err := simDB.Query(
		`SELECT timestamp, rating, note, source
		 FROM sim_mood_entries WHERE run_id = ? ORDER BY id ASC`, runID,
	)
	if err != nil {
		return
	}
	defer rows.Close()

	type moodRow struct {
		ts     string
		rating int
		note   sql.NullString
		source sql.NullString
	}
	var moods []moodRow
	for rows.Next() {
		var m moodRow
		if err := rows.Scan(&m.ts, &m.rating, &m.note, &m.source); err != nil {
			continue
		}
		moods = append(moods, m)
	}

	fmt.Fprintf(b, "## Mood Entries (%d)\n\n", len(moods))
	if len(moods) > 0 {
		b.WriteString("| Time | Rating | Note | Source |\n")
		b.WriteString("|------|--------|------|--------|\n")
		for _, m := range moods {
			note := ""
			if m.note.Valid {
				note = m.note.String
			}
			source := "inferred"
			if m.source.Valid {
				source = m.source.String
			}
			fmt.Fprintf(b, "| %s | %d | %s | %s |\n", m.ts, m.rating, note, source)
		}
	}
	b.WriteString("\n")
}

// writeSummariesSection writes any compaction summaries to the report.
// Each summary represents a point where older conversation history was
// compressed to stay within the token budget.
func writeSummariesSection(b *strings.Builder, simDB *sql.DB, runID int64) {
	rows, err := simDB.Query(
		`SELECT timestamp, summary, messages_summarized
		 FROM sim_summaries WHERE run_id = ? ORDER BY id ASC`, runID,
	)
	if err != nil {
		return
	}
	defer rows.Close()

	type summaryRow struct {
		ts               string
		summary          string
		msgsSummarized   int
	}
	var summaries []summaryRow
	for rows.Next() {
		var s summaryRow
		if err := rows.Scan(&s.ts, &s.summary, &s.msgsSummarized); err != nil {
			continue
		}
		summaries = append(summaries, s)
	}

	fmt.Fprintf(b, "## Compaction Events (%d)\n\n", len(summaries))
	if len(summaries) == 0 {
		b.WriteString("_No compaction triggered during this run._\n\n")
	} else {
		for i, s := range summaries {
			fmt.Fprintf(b, "### Compaction %d (%s) — %d messages summarized\n\n", i+1, s.ts, s.msgsSummarized)
			b.WriteString("```\n")
			b.WriteString(s.summary)
			b.WriteString("\n```\n\n")
		}
	}
}

// writeCostSection writes the cost summary table grouped by model.
func writeCostSection(b *strings.Builder, simDB *sql.DB, runID int64) {
	rows, err := simDB.Query(
		`SELECT model,
		        COUNT(*) as calls,
		        SUM(prompt_tokens) as prompt,
		        SUM(completion_tokens) as completion,
		        SUM(total_tokens) as total,
		        SUM(cost_usd) as cost
		 FROM sim_metrics WHERE run_id = ?
		 GROUP BY model ORDER BY cost DESC`, runID,
	)
	if err != nil {
		return
	}
	defer rows.Close()

	type costRow struct {
		model      string
		calls      int
		prompt     int
		completion int
		total      int
		cost       float64
	}
	var costs []costRow
	for rows.Next() {
		var c costRow
		if err := rows.Scan(&c.model, &c.calls, &c.prompt, &c.completion, &c.total, &c.cost); err != nil {
			continue
		}
		costs = append(costs, c)
	}

	b.WriteString("## Cost Summary\n\n")
	if len(costs) > 0 {
		b.WriteString("| Model | Calls | Prompt | Completion | Total | Cost |\n")
		b.WriteString("|-------|-------|--------|------------|-------|------|\n")
		for _, c := range costs {
			fmt.Fprintf(b, "| %s | %d | %d | %d | %d | $%.4f |\n",
				c.model, c.calls, c.prompt, c.completion, c.total, c.cost)
		}
	}
	b.WriteString("\n")
}

// truncate shortens a string to maxLen characters, appending "..." if it
// was truncated. Useful for keeping report output readable.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	// In Go, slicing a string by byte index is fine for ASCII. For full
	// Unicode safety you'd convert to []rune first, but for tool args
	// and debug output this is good enough.
	return s[:maxLen] + "..."
}

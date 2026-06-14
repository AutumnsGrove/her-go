// Package workeragent implements a general-purpose background agent that
// executes tasks and produces file artifacts. It runs its own tool-calling
// loop (like the memory and introspection agents) but is designed for
// work that produces output files: briefings, research reports, and
// (in future) code.
//
// Each task type has its own prompt.md and meta.yaml under tasks/<type>/.
// Adding a new task type requires zero Go code — just create the directory
// with those two files.
//
// The worker is triggered two ways:
//   - Scheduler: a cron-fired handler calls RunWorker directly
//   - Driver delegation: send_task(target="worker") fires it in a goroutine
package workeragent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	engine "her/agent_engine"
	"her/config"
	"her/embed"
	"her/gmail"
	"her/llm"
	"her/logger"
	"her/memory"
	"her/search"
	"her/tools"
	"her/tui"

	// Blank imports register tool handlers the worker uses.
	_ "her/tools/done"
	_ "her/tools/list_files"
	_ "her/tools/patch_file"
	_ "her/tools/read_email"
	_ "her/tools/read_file"
	_ "her/tools/search_emails"
	_ "her/tools/summary"
	_ "her/tools/think"
	_ "her/tools/web_read"
	_ "her/tools/web_search"
	_ "her/tools/write_file"
)

var log = logger.WithPrefix("worker")

// WorkerInput describes what to work on.
type WorkerInput struct {
	// TaskType selects the prompt and model tier (e.g. "briefing", "research").
	TaskType string

	// Instruction is freeform text from the scheduler payload or the
	// driver's send_task note. Injected into the prompt as {{instruction}}.
	Instruction string

	// Payload carries structured key-value data from the scheduler or
	// driver. Available in the prompt as {{payload.<key>}}.
	Payload map[string]string

	// TriggerMsgID links this worker run to the parent conversation turn.
	// Used to save agent_turns so the worker's tool calls appear in sim
	// reports alongside the driver's trace.
	TriggerMsgID int64
}

// WorkerParams bundles the dependencies the worker agent needs.
type WorkerParams struct {
	LLM          *llm.Client
	TavilyClient *search.TavilyClient
	EmbedClient  *embed.Client
	Store        memory.Store
	Cfg          *config.Config
	ReportsDir   string // absolute path to reports/

	// GmailBridge provides email access for email-related task types.
	// Nil means email tools return "not configured".
	GmailBridge gmail.Bridge

	// Trace callbacks — same pattern as memory agent.
	TraceCallback tools.TraceCallback // nil = full tracing disabled
	LiteToolHook  func(toolName string) // nil = lite tracing disabled
	EventBus      *tui.Bus              // nil-safe
}

// WorkerResult holds the outcome of a worker run.
type WorkerResult struct {
	ReportPath   string  // local file path (empty if no file written)
	TelegraphURL string  // published URL (empty if publishing skipped/failed)
	Title        string  // first markdown heading, or task type
	Summary      string  // from the done tool's summary arg
	CostUSD      float64
	ToolCalls    int
	Success      bool
}

const (
	defaultMaxIterations    = 20
	defaultMaxContinuations = 2
)

// RunWorker executes a task using the worker agent loop. It loads the
// per-task-type prompt, builds a scoped tool set, and runs a tool-calling
// loop that produces file artifacts in the reports directory.
func RunWorker(input WorkerInput, params WorkerParams) WorkerResult {
	if params.LLM == nil {
		log.Error("worker agent: no LLM configured")
		return WorkerResult{}
	}

	log.Info("─── worker agent ───", "task", input.TaskType)

	// Look up the task type to get its prompt.
	taskType := Lookup(input.TaskType)
	if taskType == nil {
		log.Error("worker agent: unknown task type", "type", input.TaskType)
		return WorkerResult{Summary: fmt.Sprintf("unknown task type: %s", input.TaskType)}
	}

	// Load the prompt, expanding placeholders.
	promptContent := taskType.LoadPrompt(params.Cfg, input.Instruction, input.Payload)

	// Ensure reports directory exists.
	if err := os.MkdirAll(params.ReportsDir, 0755); err != nil {
		log.Error("worker agent: creating reports dir", "err", err)
		return WorkerResult{Summary: fmt.Sprintf("failed to create reports dir: %v", err)}
	}

	// Build a minimal tools.Context — only the fields worker tools need.
	workerToolDefs := tools.ToolDefsForAgent("worker", params.Cfg)
	tctx := &tools.Context{
		AgentName:    "worker",
		Store:        params.Store,
		EmbedClient:  params.EmbedClient,
		TavilyClient: params.TavilyClient,
		GmailBridge:  params.GmailBridge,
		Cfg:          params.Cfg,
		ReportsDir:   params.ReportsDir,
		ActiveTools:  &workerToolDefs,
	}

	// Loop limits from config with sensible defaults.
	iterLimit := defaultMaxIterations
	contLimit := defaultMaxContinuations
	if params.Cfg != nil {
		if params.Cfg.WorkerAgent.MaxIterations > 0 {
			iterLimit = params.Cfg.WorkerAgent.MaxIterations
		}
		if params.Cfg.WorkerAgent.MaxContinuations > 0 {
			contLimit = params.Cfg.WorkerAgent.MaxContinuations
		}
	}

	// Track turn index for agent_turns recording — same pattern as the
	// driver agent's PostTool in agent/agent.go.
	var turnIndex int
	msgID := input.TriggerMsgID

	// Run the tool-calling loop via the shared engine.
	loopResult, err := engine.RunLoop(engine.EngineConfig{
		Name:                "worker",
		MetricRole:          "worker",
		LLM:                 params.LLM,
		Store:               params.Store,
		ToolDefs:            workerToolDefs,
		ToolCtx:             tctx,
		IterationsPerWindow: iterLimit,
		MaxContinuations:    contLimit,
		TraceCallback:       params.TraceCallback,
		LiteToolHook:        params.LiteToolHook,
		EventBus:            params.EventBus,
		Messages: []llm.ChatMessage{
			{Role: "system", Content: promptContent},
			{Role: "user", Content: buildWorkerInstruction(input)},
		},

		// Record every tool call to agent_turns so worker activity is
		// visible in sim reports and traces — same as the driver does.
		PostTool: func(tc llm.ToolCall, result string, isError bool) {
			if params.Store == nil || msgID == 0 {
				return
			}
			// Prefix tool names with [worker] so traces distinguish
			// worker calls from driver calls on the same turn.
			prefixed := "[worker] " + tc.Function.Name
			params.Store.SaveAgentTurn(msgID, turnIndex, "assistant", prefixed, tc.Function.Arguments, "")
			turnIndex++
			params.Store.SaveAgentTurn(msgID, turnIndex, "tool", prefixed, "", result)
			turnIndex++
		},

		ContinuationMsg: func(window, maxWindows int, summary string) string {
			return fmt.Sprintf(
				"You have used all %d iterations in the previous window without calling done. "+
					"Continuation window %d of %d. Your progress so far:\n%s\n\n"+
					"Finish up and call done with a summary of your work.",
				iterLimit, window, maxWindows, summary,
			)
		},
	})
	if err != nil {
		log.Error("worker agent: engine error", "err", err)
		return WorkerResult{Summary: fmt.Sprintf("engine error: %v", err)}
	}

	// Extract results — find the report file(s) written and the done summary.
	workerResult := WorkerResult{
		CostUSD:   loopResult.TotalCost,
		ToolCalls: loopResult.ToolCalls,
		Success:   tctx.DoneCalled,
	}

	// Use the last file the worker actually wrote (tracked by write_file
	// tool via ctx.WrittenFiles), not the newest file in reports/ by
	// timestamp. The old findLatestReport approach would attach unrelated
	// reports from previous runs.
	if len(tctx.WrittenFiles) > 0 {
		workerResult.ReportPath = tctx.WrittenFiles[len(tctx.WrittenFiles)-1]
		workerResult.Title = extractTitle(workerResult.ReportPath)
	}
	if workerResult.Title == "" {
		workerResult.Title = input.TaskType
	}

	// Extract summary in priority order:
	// 1. summary tool (dedicated "here's my report" call)
	// 2. done(summary=...) for backward compat with older task prompts
	// 3. last think() as final fallback — models produce good analysis
	//    there even when they fail at the protocol layer
	workerResult.Summary = tctx.WorkerSummary
	if workerResult.Summary == "" {
		workerResult.Summary = extractDoneSummary(loopResult.Messages)
	}
	if workerResult.Summary == "" {
		workerResult.Summary = extractLastThink(loopResult.Messages)
	}

	log.Infof("  worker agent: task=%s | report=%s | $%.6f | %d tools",
		input.TaskType, filepath.Base(workerResult.ReportPath), loopResult.TotalCost, loopResult.ToolCalls)

	return workerResult
}

// buildWorkerInstruction creates the user message for the worker agent.
func buildWorkerInstruction(input WorkerInput) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Task type: %s\n", input.TaskType))
	if input.Instruction != "" {
		sb.WriteString(fmt.Sprintf("\nInstruction: %s\n", input.Instruction))
	}
	if len(input.Payload) > 0 {
		sb.WriteString("\nParameters:\n")
		for k, v := range input.Payload {
			sb.WriteString(fmt.Sprintf("  %s: %s\n", k, v))
		}
	}
	sb.WriteString(fmt.Sprintf("\nToday's date: %s\n", time.Now().Format("2006-01-02")))
	return sb.String()
}


// findLatestReport finds the most recently modified file in the reports dir.
func findLatestReport(dir string) string {
	var latest string
	var latestTime time.Time

	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() == ".gitkeep" {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		if info.ModTime().After(latestTime) {
			latestTime = info.ModTime()
			latest = path
		}
		return nil
	})
	return latest
}

// extractTitle reads the first markdown heading from a report file.
func extractTitle(path string) string {
	content, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(content), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, "# "))
		}
	}
	return ""
}

// extractLastThink returns the content of the last think() call in the
// message history. Used as a fallback summary when done() wasn't called
// or its args were truncated — the think trace usually contains the
// worker's complete analysis.
func extractLastThink(messages []llm.ChatMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role != "assistant" || len(msg.ToolCalls) == 0 {
			continue
		}
		for j := len(msg.ToolCalls) - 1; j >= 0; j-- {
			tc := msg.ToolCalls[j]
			if tc.Function.Name == "think" {
				var args struct {
					Thought string `json:"thought"`
				}
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err == nil && args.Thought != "" {
					return args.Thought
				}
			}
		}
	}
	return ""
}

// extractDoneSummary pulls the summary from the last done tool call in
// the message history.
func extractDoneSummary(messages []llm.ChatMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role != "assistant" || len(msg.ToolCalls) == 0 {
			continue
		}
		for _, tc := range msg.ToolCalls {
			if tc.Function.Name == "done" {
				var args struct {
					Summary string `json:"summary"`
				}
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err == nil {
					return args.Summary
				}
			}
		}
	}
	return ""
}


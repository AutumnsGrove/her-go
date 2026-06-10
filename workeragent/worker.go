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

	"her/config"
	"her/embed"
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
	_ "her/tools/read_file"
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
}

// WorkerParams bundles the dependencies the worker agent needs.
type WorkerParams struct {
	LLM          *llm.Client
	TavilyClient *search.TavilyClient
	EmbedClient  *embed.Client
	Store        memory.Store
	Cfg          *config.Config
	ReportsDir   string // absolute path to reports/

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
	tctx := &tools.Context{
		AgentName:    "worker",
		Store:        params.Store,
		EmbedClient:  params.EmbedClient,
		TavilyClient: params.TavilyClient,
		Cfg:          params.Cfg,
		ReportsDir:   params.ReportsDir,
		ActiveTools:  nil, // worker uses all its tools as hot
	}

	// Tool definitions for the worker agent.
	workerToolDefs := tools.ToolDefsForAgent("worker", params.Cfg)
	tctx.ActiveTools = &workerToolDefs

	// Build the initial message list.
	messages := []llm.ChatMessage{
		{Role: "system", Content: promptContent},
		{Role: "user", Content: buildWorkerInstruction(input)},
	}

	var totalCost float64
	var totalToolCalls int

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

	// Trace setup — same dual-mode pattern as the memory agent.
	tracing := params.TraceCallback != nil
	var traceLines []string

	sendTrace := func() {
		if !tracing || len(traceLines) == 0 {
			return
		}
		_ = params.TraceCallback(strings.Join(traceLines, "\n"))
	}

outer:
	for window := 0; window <= contLimit; window++ {
		if window > 0 {
			summary := buildWorkerContinuationSummary(traceLines)
			messages = append(messages, llm.ChatMessage{
				Role: "system",
				Content: fmt.Sprintf(
					"You have used all %d iterations in the previous window without calling done. "+
						"Continuation window %d of %d. Your progress so far:\n%s\n\n"+
						"Finish up and call done with a summary of your work.",
					iterLimit, window, contLimit, summary,
				),
			})
			log.Infof("  [worker] continuation window %d/%d", window, contLimit)
			if tracing {
				traceLines = append(traceLines, fmt.Sprintf(
					"🔄 <i>continuation window %d/%d</i>", window, contLimit))
				sendTrace()
			}
		}

		for i := 0; i < iterLimit; i++ {
			resp, err := params.LLM.ChatCompletionWithTools(messages, workerToolDefs)
			if err != nil {
				log.Error("worker agent: LLM error", "err", err, "iteration", i)
				if tracing {
					traceLines = append(traceLines, fmt.Sprintf("❌ <b>error:</b> %s", truncateLog(err.Error(), 100)))
					sendTrace()
				}
				break outer
			}

			params.Store.SaveMetric(resp.Model, resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens, resp.CostUSD, 0, 0, resp.UsedFallback, "worker")
			totalCost += resp.CostUSD
			log.Infof("  [worker] tokens: %d prompt + %d completion | $%.6f | finish=%s",
				resp.PromptTokens, resp.CompletionTokens, resp.CostUSD, resp.FinishReason)

			if len(resp.ToolCalls) == 0 {
				if resp.Content != "" {
					log.Infof("  [worker] text response (no tool calls): %s", truncateLog(resp.Content, 200))
				}
				break outer
			}

			messages = append(messages, llm.ChatMessage{
				Role:      "assistant",
				Content:   resp.Content,
				ToolCalls: resp.ToolCalls,
			})

			for _, tc := range resp.ToolCalls {
				if tc.Function.Name == "" {
					continue
				}
				totalToolCalls++

				result := tools.Execute(tc.Function.Name, tc.Function.Arguments, tctx)
				isError := strings.HasPrefix(result, "error:")

				log.Infof("    [worker] %s → %s", tc.Function.Name, truncateLog(result, 150))

				if params.LiteToolHook != nil {
					params.LiteToolHook(tc.Function.Name)
				}

				if params.EventBus != nil {
					params.EventBus.Emit(tui.ToolCallEvent{
						Time:     time.Now(),
						Source:   "worker",
						ToolName: tc.Function.Name,
						Args:     truncateLog(tc.Function.Arguments, 200),
						Result:   truncateLog(result, 200),
						IsError:  isError,
					})
				}

				if tracing {
					line := tools.FormatTrace(tc.Function.Name, tc.Function.Arguments, result)
					traceLines = append(traceLines, line)
					sendTrace()
				}

				messages = append(messages, llm.ChatMessage{
					Role:       "tool",
					Content:    result,
					ToolCallID: tc.ID,
				})
			}

			if tctx.DoneCalled {
				log.Info("  [worker] done signal received")
				break outer
			}

			if resp.FinishReason == "stop" {
				log.Info("  [worker] finish_reason=stop after tool execution")
				break outer
			}
		}

		if window == contLimit {
			log.Warn("[worker] hit max continuations without done signal")
			if tracing {
				traceLines = append(traceLines, "⚠️ <i>max continuations reached</i>")
				sendTrace()
			}
			break outer
		}
	}

	// Extract results — find the report file(s) written and the done summary.
	result := WorkerResult{
		CostUSD:   totalCost,
		ToolCalls: totalToolCalls,
		Success:   tctx.DoneCalled,
	}

	// Find the most recently written report file.
	result.ReportPath = findLatestReport(params.ReportsDir)
	if result.ReportPath != "" {
		result.Title = extractTitle(result.ReportPath)
	}
	if result.Title == "" {
		result.Title = input.TaskType
	}

	// Extract the summary from the done tool call (last message with done args).
	result.Summary = extractDoneSummary(messages)

	log.Infof("  worker agent: task=%s | report=%s | $%.6f | %d tools",
		input.TaskType, filepath.Base(result.ReportPath), totalCost, totalToolCalls)

	return result
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

// buildWorkerContinuationSummary extracts tool names from trace lines for
// the continuation context injection.
func buildWorkerContinuationSummary(traceLines []string) string {
	if len(traceLines) == 0 {
		return "(no progress recorded)"
	}
	return strings.Join(traceLines, "\n")
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

func truncateLog(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

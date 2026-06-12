package workeragent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"her/agent"
	"her/config"
	"her/llm"
	"her/memory"
	"her/scheduler"
	"her/search"
	"her/telegraph"
)

// briefingHandler implements scheduler.Handler for cron-fired briefings.
// It runs the worker agent with the "briefing" task type and emits an
// event when done so the driver agent can comment on the report.
type briefingHandler struct{}

func (briefingHandler) Kind() string       { return "worker_briefing" }
func (briefingHandler) ConfigPath() string { return "workeragent/tasks/briefing/task.yaml" }

func (h briefingHandler) Execute(ctx context.Context, payload json.RawMessage, deps *scheduler.Deps) error {
	var p struct {
		Topics      string `json:"topics"`
		Instruction string `json:"instruction"`
		Depth       string `json:"depth"`      // "brief" or "deep" — maps to model tier
		ModelTier   string `json:"model_tier"` // explicit tier override (wins over depth)
		Detail      string `json:"detail"`     // "brief", "default", or "detailed" — report verbosity
	}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &p); err != nil {
			log.Warn("briefing handler: malformed payload, using defaults", "err", err)
		}
	}

	instruction := p.Instruction
	if instruction == "" && p.Topics != "" {
		instruction = fmt.Sprintf("Search for news and updates about: %s", p.Topics)
	}
	if instruction == "" {
		instruction = "Complete the briefing task as described in your system prompt."
	}

	// Append detail-level directive to shape report verbosity.
	switch p.Detail {
	case "brief":
		instruction += "\n\nIMPORTANT: Keep it concise. Bullet points only, 5-8 bullets max. No lengthy paragraphs or deep analysis — just the key headlines and takeaways."
	case "detailed":
		instruction += "\n\nWrite a comprehensive, detailed report with full analysis. Include context, implications, and connections between developments. Aim for depth over brevity."
	}
	// "default" or empty = no modifier, normal report length

	// Resolve dependencies from the scheduler's Deps. These are typed as
	// `any` in Deps to avoid import cycles — we cast here.
	cfg, _ := deps.Cfg.(*config.Config)
	if cfg == nil {
		return fmt.Errorf("worker briefing: no config available")
	}

	taskType := Lookup("briefing")
	if taskType == nil {
		return fmt.Errorf("worker briefing: task type 'briefing' not registered")
	}

	// Select the LLM client — tier override chain:
	// meta.yaml default → depth mapping → explicit model_tier.
	workerLLMs, _ := deps.WorkerLLMs.(map[string]*llm.Client)
	if workerLLMs == nil {
		return fmt.Errorf("worker briefing: no worker LLMs configured")
	}
	tier := taskType.ModelTier
	switch p.Depth {
	case "low", "medium", "high":
		tier = p.Depth
	case "deep":
		tier = "medium"
	case "brief":
		tier = "low"
	}
	if p.ModelTier != "" {
		tier = p.ModelTier
	}
	llmClient := workerLLMs[tier]
	if llmClient == nil {
		return fmt.Errorf("worker briefing: no LLM for tier %q", tier)
	}

	tavilyClient, _ := deps.TavilyClient.(*search.TavilyClient)
	store, _ := deps.Store.(memory.Store)

	reportsDir := filepath.Join(deps.RootDir, "reports")
	if cfg.WorkerAgent.ReportsDir != "" {
		reportsDir = filepath.Join(deps.RootDir, cfg.WorkerAgent.ReportsDir)
	}

	// Run the worker.
	result := RunWorker(WorkerInput{
		TaskType:    "briefing",
		Instruction: instruction,
	}, WorkerParams{
		LLM:          llmClient,
		TavilyClient: tavilyClient,
		Store:        store,
		Cfg:          cfg,
		ReportsDir:   reportsDir,
	})

	// Publish to Telegraph if configured.
	if cfg.WorkerAgent.TelegraphToken != "" && result.ReportPath != "" {
		tc := telegraph.NewClient(cfg.WorkerAgent.TelegraphToken, cfg.Identity.Her)
		content, err := os.ReadFile(result.ReportPath)
		if err == nil {
			url, pubErr := tc.CreatePage(result.Title, string(content))
			if pubErr != nil {
				log.Warn("telegraph publish failed", "err", pubErr)
			} else {
				result.TelegraphURL = url
			}
		}
	}

	// Emit event so the driver agent can comment on the report.
	// Try both chan and chan<- types — cmd/run.go passes chan<- but tests
	// might pass the bidirectional channel.
	var eventCh chan<- agent.AgentEvent
	if ch, ok := deps.AgentEventCh.(chan<- agent.AgentEvent); ok {
		eventCh = ch
	} else if ch, ok := deps.AgentEventCh.(chan agent.AgentEvent); ok {
		eventCh = ch
	}
	if eventCh != nil {
		evt := agent.AgentEvent{
			Type:      agent.EventWorkerComplete,
			TaskName:  "briefing",
			Summary:   result.Summary,
			ReportURL: result.TelegraphURL,
			Timestamp: time.Now(),
		}
		select {
		case eventCh <- evt:
		default:
			log.Warn("agent event channel full, dropping worker completion event")
		}
	}

	if !result.Success {
		return fmt.Errorf("worker briefing did not complete (no done signal)")
	}
	return nil
}

func init() {
	scheduler.Register(briefingHandler{})
}

package testutil

import (
	"testing"

	"her/config"
	"her/scrub"
	"her/tools"
)

// TestToolContext assembles a fully-wired tools.Context suitable for
// integration-testing individual tool handlers. It includes:
//
//   - A real SQLite store (temp DB, auto-cleaned)
//   - A stub embed client (deterministic hash-based vectors)
//   - A stub chat LLM (must pass canned responses if the tool under test calls it)
//   - A minimal config with sensible defaults
//   - A fresh PII scrub vault
//   - No-op callbacks (status, send, trace, etc.)
//
// Optional services (VisionLLM, ClassifierLLM, TavilyClient, etc.) are
// left nil — set them on the returned Context if the test needs them.
//
// Usage:
//
//	ctx := testutil.TestToolContext(t)
//	result := tools.Execute("get_current_time", `{}`, ctx)
func TestToolContext(t *testing.T) *tools.Context {
	t.Helper()

	store := TempStore(t)
	embedClient := StubEmbedClient(t)

	// Minimal config — just the fields that tool handlers actually read.
	// Everything else stays at zero values, which is fine for tests.
	cfg := &config.Config{
		Identity: config.IdentityConfig{
			Her:  "TestBot",
			User: "TestUser",
		},
		Memory: config.MemoryConfig{
			RecentMessages:     20,
			MaxFactsInContext:  10,
			MaxHistoryTokens:   4000,
			AgentContextBudget: 8000,
			MaxFactRetries:     3,
		},
		Scheduler: config.SchedulerConfig{
			Timezone: "UTC",
		},
	}

	return &tools.Context{
		Store:       store,
		EmbedClient: embedClient,
		Cfg:         cfg,
		ScrubVault:  scrub.NewVault(),

		SimilarityThreshold: 0.85,
		ConversationID:      "test-conv-1",

		// No-op callbacks — tools can call these without nil panics.
		// In Python you'd pass lambda: None. In Go, same idea: a
		// function literal that does nothing and returns no error.
		StatusCallback: func(status string) error { return nil },
		SendCallback:   func(text string) error { return nil },
		TTSCallback:    func(text string) {},
		TraceCallback:  func(text string) error { return nil },
		StageResetCallback: func() error { return nil },
		DeletePlaceholderCallback: func() error { return nil },

		// Mutable state — starts clean each test.
		PreApprovedRewrites: make(map[string]bool),
	}
}

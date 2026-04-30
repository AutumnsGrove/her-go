package scheduler

import (
	"context"
	"encoding/json"
	"time"
)

// Handler is implemented by each scheduler extension. Extensions live in
// their domain package (e.g. `mood/rollup_task.go`) and self-register via
// Register() at init() time — same pattern the `tools/` package uses.
//
// Each handler owns exactly one Kind. The scheduler persists one row per
// kind; when its next_fire arrives, the scheduler dispatches to this
// handler. The handler interprets the payload however it needs.
//
// Go note: this is a regular Go interface. Satisfaction is implicit — any
// type with all three methods is a Handler. No `implements` keyword
// needed, just like Python's duck typing but checked at compile time.
type Handler interface {
	// Kind is the stable string identifier for this task type. Used as
	// the primary discriminator in the scheduler_tasks table and for
	// registry lookup. Example: "mood_daily_rollup".
	Kind() string

	// ConfigPath returns the path (relative to the project root) of the
	// task.yaml file that declares this task's cron expression, payload,
	// and retry policy. Loader reads this at startup and upserts the DB
	// row accordingly.
	//
	// Empty string is allowed for handlers that don't ship a YAML (e.g.
	// tests with hand-crafted registrations).
	ConfigPath() string

	// Execute runs the task. Payload is the raw JSON from the DB row,
	// which originated in task.yaml. Deps carries shared dependencies
	// (store, Telegram send, LLM client, etc.). The ctx is cancelled on
	// scheduler shutdown — handlers should respect it.
	Execute(ctx context.Context, payload json.RawMessage, deps *Deps) error
}

// Deps bundles the shared dependencies every handler might want. Fields
// get added as extensions need them; handlers only touch what they use.
//
// Go note: this is a plain struct, not an interface. Simpler than defining
// a per-extension interface, and extensions are internal to the project so
// we don't need the abstraction.
type Deps struct {
	// Store backs every extension's DB needs — scheduler tasks, mood
	// entries, facts, whatever the handler reads/writes. It's the
	// project's central SQLite wrapper.
	//
	// Typed as `any` here to avoid a scheduler → memory import cycle.
	// Handlers cast to `*memory.SQLiteStore` at the call site. Not beautiful,
	// but contained: every handler's first line is usually the cast.
	Store any

	// Send pushes a plain-text Telegram message and returns the delivered
	// message's ID (so handlers can edit it later, e.g. proposal expiry).
	Send func(chatID int64, text string) (int, error)

	// SendPNG pushes a PNG image as a Telegram photo reply with an
	// optional caption. Used by the /mood chart commands and rollup
	// summaries.
	SendPNG func(chatID int64, png []byte, caption string) error

	// ChatID is the owner's Telegram chat ID — the primary user. Almost
	// every extension that sends proactive messages targets this.
	ChatID int64
}

// RetryConfig governs what happens when a handler returns an error.
// Parsed from task.yaml and stored as three plain columns on the
// scheduler_tasks row (no JSON indirection for such a small shape).
type RetryConfig struct {
	// MaxAttempts is the total number of times to run the handler before
	// giving up. 0 means "no retry" — any error moves straight to the
	// next scheduled fire (for recurring tasks) or deletion (one-shot).
	MaxAttempts int `yaml:"max_attempts"`

	// Backoff selects the wait strategy between attempts:
	//   "none"        — retry immediately on the next tick
	//   "linear"      — wait = InitialWait * attempt
	//   "exponential" — wait = InitialWait * 2^(attempt-1)
	Backoff string `yaml:"backoff"`

	// InitialWait is the base delay used by the "linear" and
	// "exponential" backoff strategies. Ignored when Backoff="none".
	InitialWait time.Duration `yaml:"initial_wait"`
}

// Valid returns true if the backoff string is a recognized value.
func (r RetryConfig) Valid() bool {
	switch r.Backoff {
	case "", "none", "linear", "exponential":
		return true
	}
	return false
}

// NextWait returns the delay to apply after a given failed attempt
// number (1-indexed). For attempt=1, it's the wait before retrying the
// first time, etc.
func (r RetryConfig) NextWait(attempt int) time.Duration {
	if r.InitialWait <= 0 || attempt < 1 {
		return 0
	}
	switch r.Backoff {
	case "linear":
		return r.InitialWait * time.Duration(attempt)
	case "exponential":
		// shift by attempt-1 so attempt=1 → 1x, attempt=2 → 2x, attempt=3 → 4x …
		return r.InitialWait << (attempt - 1)
	default:
		return 0
	}
}

// TaskConfig is the on-disk shape of a task.yaml file. The loader parses
// this, combines it with the registered handler's Kind, and upserts the
// scheduler_tasks row.
type TaskConfig struct {
	// Kind must match the registered handler's Kind(). Parser validates
	// this at load time and errors if there's a mismatch — catches typos.
	Kind string `yaml:"kind"`

	// Cron is a standard 5-field expression ("m h dom mon dow") parsed
	// by robfig/cron/v3. Empty means one-shot — the scheduler fires once
	// at NextFire time and deletes the row.
	Cron string `yaml:"cron"`

	// Payload is forwarded verbatim to Handler.Execute as JSON. Use this
	// to parameterize reusable handlers (e.g. a generic "send_message"
	// handler that takes {"text": "..."} from here).
	//
	// Typed as `any` because gopkg.in/yaml.v3 can't unmarshal a YAML map
	// directly into json.RawMessage — the loader re-marshals this to
	// JSON before handing it to the store/handler.
	Payload any `yaml:"payload"`

	// Retry controls failure behavior. Omitting it in YAML means "no retry".
	Retry RetryConfig `yaml:"retry"`
}

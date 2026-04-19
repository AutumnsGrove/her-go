package scheduler

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"her/memory"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
)

// loadAndUpsertAll walks the registered handlers, reads each one's
// task.yaml from disk, and upserts the corresponding row in the
// scheduler_tasks table. Called once from New() at startup.
//
// If a handler's ConfigPath() is empty, it's skipped — that handler is
// registered purely in code (typically a test). An unregistered kind
// already in the DB is left alone; the runner will log + skip it when
// it fires.
//
// rootDir is the project root; ConfigPath() is resolved relative to it.
func (s *Scheduler) loadAndUpsertAll(rootDir string) error {
	parser := cron.NewParser(
		cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow,
	)

	for _, kind := range registeredKinds() {
		h := lookup(kind)
		path := h.ConfigPath()
		if path == "" {
			s.log.Debug("no task.yaml for handler; skipping load", "kind", kind)
			continue
		}

		absPath := filepath.Join(rootDir, path)
		cfg, err := readTaskConfig(absPath)
		if err != nil {
			return fmt.Errorf("scheduler: reading %s: %w", absPath, err)
		}

		if cfg.Kind != kind {
			return fmt.Errorf(
				"scheduler: %s declares kind %q but handler kind is %q",
				absPath, cfg.Kind, kind,
			)
		}
		if !cfg.Retry.Valid() {
			return fmt.Errorf(
				"scheduler: %s has invalid retry.backoff %q (want none|linear|exponential)",
				absPath, cfg.Retry.Backoff,
			)
		}

		nextFire, err := computeNextFire(parser, cfg.Cron, time.Now())
		if err != nil {
			return fmt.Errorf("scheduler: computing next_fire for %s: %w", kind, err)
		}

		// If a row already exists, preserve its next_fire — otherwise
		// every restart would skip to "one cron tick from now" and the
		// task could miss fires that were due during downtime.
		existing, err := s.store.SchedulerTaskByKind(kind)
		if err != nil {
			return fmt.Errorf("scheduler: looking up existing %s: %w", kind, err)
		}
		if existing != nil {
			// Keep next_fire from DB unless the cron expression changed —
			// a cron-expression edit deserves a fresh schedule.
			if existing.CronExpr == cfg.Cron {
				nextFire = existing.NextFire
			}
		}

		payload := cfg.Payload
		if len(payload) == 0 {
			payload = json.RawMessage("{}")
		}

		task := &memory.SchedulerTask{
			Kind:             kind,
			CronExpr:         cfg.Cron,
			NextFire:         nextFire,
			Payload:          payload,
			RetryMaxAttempts: cfg.Retry.MaxAttempts,
			RetryBackoff:     backoffOrNone(cfg.Retry.Backoff),
			RetryInitialWait: cfg.Retry.InitialWait,
		}

		if err := s.store.UpsertSchedulerTask(task); err != nil {
			return fmt.Errorf("scheduler: upserting %s: %w", kind, err)
		}
		s.log.Info("scheduler task loaded",
			"kind", kind,
			"cron", cfg.Cron,
			"next_fire", nextFire.Format(time.RFC3339),
		)
	}
	return nil
}

// readTaskConfig parses a task.yaml file at the given absolute path.
func readTaskConfig(path string) (*TaskConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var cfg TaskConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &cfg, nil
}

// computeNextFire returns the next scheduled fire time from now. An
// empty cron string means one-shot and is not supported yet by the
// loader (one-shot tasks are created programmatically, not via YAML).
func computeNextFire(parser cron.Parser, expr string, now time.Time) (time.Time, error) {
	if expr == "" {
		return time.Time{}, fmt.Errorf("empty cron expression")
	}
	sched, err := parser.Parse(expr)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing cron %q: %w", expr, err)
	}
	return sched.Next(now), nil
}

// backoffOrNone normalizes an empty Backoff to "none" so the DB column
// never holds an empty string (makes queries and future reads simpler).
func backoffOrNone(b string) string {
	if b == "" {
		return "none"
	}
	return b
}

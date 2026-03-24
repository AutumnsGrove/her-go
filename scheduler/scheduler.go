// Package scheduler runs scheduled tasks — reminders, cron jobs, and
// proactive messages. It's a goroutine-based ticker loop that polls
// the database every 30 seconds for tasks whose next_run time has passed.
//
// Supports one-shot reminders, recurring cron jobs, and conditional tasks.
// Task types include "send_message" (plain text) and "run_prompt" (full
// agent pipeline). Damping controls (quiet hours, rate limiting,
// conversation-aware deferral) prevent proactive messages from being
// annoying. The priority system (normal/high/critical) ensures important
// tasks like user reminders and medication check-ins always get through.
//
// Design philosophy: the scheduler is a "dumb executor with a smart
// payload." It wakes up, finds due tasks, executes them by type, and
// computes the next run time. All intelligence lives in task payloads.
package scheduler

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"her/logger"
	"her/memory"
)

// log is the package-level logger for the scheduler.
var log = logger.WithPrefix("scheduler")

// SendFunc is a callback the bot provides for sending Telegram messages.
// The scheduler doesn't import the bot or Telegram packages — it just
// calls this function when it needs to deliver a message. This is
// "dependency inversion" — the scheduler depends on an interface (a
// function signature), not a concrete implementation.
//
// Same idea as passing a callback in Python: scheduler doesn't care
// HOW the message gets sent, just that it does.
type SendFunc func(text string) error

// AgentFunc is a callback the bot provides for running a prompt through
// the full agent pipeline. The scheduler calls this for "run_prompt"
// tasks — the prompt goes in, the agent does its thing (search, memory,
// tool calls, etc.), and Mira's reply comes out.
//
// Same dependency inversion pattern as SendFunc: the scheduler depends
// on a function signature, not the agent package. This keeps the import
// graph clean (scheduler never imports agent, agent can import scheduler
// for cron utilities).
type AgentFunc func(prompt string) (string, error)

// SchedulerOpts holds configuration for damping and rate limiting.
// These are passed from config at startup so the scheduler doesn't
// need to import the config package directly.
type SchedulerOpts struct {
	QuietHoursStart    string        // "23:00" format, empty string = disabled
	QuietHoursEnd      string        // "07:00" format
	MaxProactivePerDay int           // 0 = unlimited
	Defaults           []DefaultTask // system tasks to create on startup if missing
}

// DefaultTask describes a system-created recurring task. The scheduler
// creates these on startup if they don't already exist in the database.
// Built from config flags in cmd/run.go.
type DefaultTask struct {
	Name     string
	CronExpr string
	TaskType string
	Priority string          // "normal", "high", "critical"
	Payload  json.RawMessage // pre-marshaled JSON
}

// Scheduler polls the database for due tasks and executes them.
// It runs in its own goroutine and communicates via context cancellation.
type Scheduler struct {
	store          *memory.Store
	sendFn         SendFunc         // sends plain text messages
	sendKeyboardFn SendKeyboardFunc // sends messages with inline keyboards — nil if not wired
	agentFn        AgentFunc        // runs prompts through the agent pipeline — nil if not wired
	location       *time.Location   // timezone for cron evaluation
	opts           *SchedulerOpts   // damping configuration
	cancel         context.CancelFunc
	wg             sync.WaitGroup
}

// New creates a scheduler. Call Start() to begin the polling loop.
//
// timezone is an IANA location string like "America/New_York".
// If empty or invalid, falls back to UTC. The timezone matters because
// when someone says "remind me at 3pm," we need to know WHICH 3pm.
//
// agentFn can be nil if the agent pipeline isn't available (e.g., during
// testing). Tasks that need it (run_prompt) will log an error and skip.
func New(store *memory.Store, sendFn SendFunc, sendKeyboardFn SendKeyboardFunc, agentFn AgentFunc, timezone string, opts SchedulerOpts) *Scheduler {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		log.Warn("invalid timezone, falling back to UTC", "timezone", timezone, "err", err)
		loc = time.UTC
	}

	return &Scheduler{
		store:          store,
		sendFn:         sendFn,
		sendKeyboardFn: sendKeyboardFn,
		agentFn:        agentFn,
		location:       loc,
		opts:           &opts,
	}
}

// Start launches the scheduler's polling loop in a background goroutine.
// It first runs startup recovery (executing missed one-shot tasks),
// then ticks every minute to check for due tasks.
//
// The context pattern here is central to Go concurrency. Instead of
// a boolean flag like `self.running = True` in Python, Go uses contexts:
//   - context.WithCancel gives us a ctx that can be "cancelled"
//   - The goroutine checks ctx.Done() to know when to stop
//   - Calling cancel() signals all goroutines watching this context
//
// This is safer than a boolean because contexts are goroutine-safe by
// design — no race conditions, no locks needed.
func (s *Scheduler) Start() {
	// Create default system tasks (morning briefing, mood check-in, etc.)
	// before starting the polling loop. This is idempotent — existing
	// tasks are left alone.
	s.ensureDefaults()

	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel

	s.wg.Add(1)
	go s.run(ctx)

	log.Info("scheduler started", "timezone", s.location.String())
}

// Stop signals the scheduler to stop and waits for it to finish.
// The WaitGroup ensures we don't return until the goroutine has
// fully exited — important for clean shutdown so we don't cut off
// a task mid-execution.
func (s *Scheduler) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
	log.Info("scheduler stopped")
}

// run is the main polling loop. It runs in its own goroutine.
func (s *Scheduler) run(ctx context.Context) {
	// defer s.wg.Done() runs when this function returns, no matter how
	// it returns (normal exit, panic, etc.). It decrements the WaitGroup
	// counter so Stop() knows we're done. Same idea as Python's finally:.
	defer s.wg.Done()

	// Startup recovery: check for one-shot tasks that were missed while
	// the bot was down. Recurring tasks just get their next_run recomputed
	// (no backfill), but one-shots fire immediately since they were
	// explicitly requested for a specific time.
	s.recoverMissedTasks()

	// time.NewTicker returns a channel that receives a value every
	// duration. It's like setInterval in JS. We read from ticker.C
	// in a select loop below.
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		// select is Go's way of waiting on multiple channels at once.
		// It blocks until one of the cases is ready, then executes that
		// case. Think of it like asyncio.wait() with FIRST_COMPLETED.
		select {
		case <-ctx.Done():
			// Context was cancelled — time to shut down.
			return
		case <-ticker.C:
			// Ticker fired — check for due tasks.
			s.tick()
		}
	}
}

// tick runs one polling cycle: find all due tasks and apply damping
// checks before executing them.
//
// The damping cascade (checked in order, using priority to decide
// which checks apply):
//
//  1. critical priority → always fire, skip ALL checks
//  2. Quiet hours       → defer normal + high tasks
//  3. high priority     → fire now (passed quiet hours)
//  4. Conversation active (last 10 min) → defer normal tasks by 30 min
//  5. Daily cap hit     → skip normal tasks, advance to next run
//
// This ensures user reminders and medication check-ins ALWAYS get
// through, while preventing chatty proactive messages from being
// annoying.
func (s *Scheduler) tick() {
	now := time.Now().In(s.location)

	tasks, err := s.store.GetDueTasks(now)
	if err != nil {
		log.Error("polling for due tasks", "err", err)
		return
	}

	for _, task := range tasks {
		// 1. Critical priority bypasses ALL damping.
		// This covers user-created reminders and medication check-ins.
		if task.Priority == "critical" {
			s.executeTask(task)
			continue
		}

		// 2. Quiet hours: defer non-critical tasks to end of quiet window.
		if quiet, endTime := s.isQuietHours(now); quiet {
			name := "<unnamed>"
			if task.Name != nil {
				name = *task.Name
			}
			log.Info("deferring task (quiet hours)",
				"id", task.ID, "name", name, "until", endTime.In(s.location))
			_ = s.store.DeferTask(task.ID, endTime)
			continue
		}

		// 3. High priority bypasses rate limits and conversation deferral.
		// (It already passed the quiet hours check above.)
		if task.Priority == "high" {
			s.executeTask(task)
			continue
		}

		// --- Everything below applies only to "normal" priority ---

		// 4. Conversation-aware deferral: if the user sent a message
		// within the last 10 minutes, defer check-ins by 30 minutes
		// to avoid interrupting an active conversation.
		lastMsg, err := s.store.LastUserMessageTime()
		if err == nil && !lastMsg.IsZero() && now.Sub(lastMsg) < 10*time.Minute {
			deferUntil := now.Add(30 * time.Minute)
			name := "<unnamed>"
			if task.Name != nil {
				name = *task.Name
			}
			log.Info("deferring task (user active)",
				"id", task.ID, "name", name, "until", deferUntil.In(s.location))
			_ = s.store.DeferTask(task.ID, deferUntil)
			continue
		}

		// 5. Daily proactive cap: skip if we've hit the limit.
		if s.opts.MaxProactivePerDay > 0 {
			count, countErr := s.store.CountTasksRunToday(now)
			if countErr == nil && count >= s.opts.MaxProactivePerDay {
				name := "<unnamed>"
				if task.Name != nil {
					name = *task.Name
				}
				log.Info("skipping task (daily proactive limit reached)",
					"id", task.ID, "name", name, "count", count, "limit", s.opts.MaxProactivePerDay)
				// For recurring tasks, advance to the next run so they
				// don't keep hitting the limit every tick.
				s.advanceToNextRun(task)
				continue
			}
		}

		// All checks passed — execute.
		s.executeTask(task)
	}
}

// advanceToNextRun skips a recurring task to its next cron run without
// executing it. Used when a task is suppressed by rate limiting.
func (s *Scheduler) advanceToNextRun(task memory.ScheduledTask) {
	if (task.ScheduleType == "recurring" || task.ScheduleType == "conditional") &&
		task.CronExpr != nil && *task.CronExpr != "" {
		now := time.Now().In(s.location)
		next, err := NextRun(*task.CronExpr, now, s.location)
		if err == nil {
			_ = s.store.DeferTask(task.ID, next)
		}
	}
}

// isQuietHours checks if the current time falls within the configured
// quiet window. Returns true + the time quiet hours end if we're in
// the window, false otherwise.
//
// Handles overnight windows (e.g., 23:00–07:00) where the start time
// is "after" the end time on the same calendar day.
func (s *Scheduler) isQuietHours(now time.Time) (bool, time.Time) {
	if s.opts.QuietHoursStart == "" || s.opts.QuietHoursEnd == "" {
		return false, time.Time{}
	}

	// Parse "23:00" and "07:00" into hour/minute components.
	startH, startM, err1 := parseHourMinute(s.opts.QuietHoursStart)
	endH, endM, err2 := parseHourMinute(s.opts.QuietHoursEnd)
	if err1 != nil || err2 != nil {
		return false, time.Time{}
	}

	// Build today's start and end times in the scheduler's timezone.
	y, m, d := now.Date()
	startToday := time.Date(y, m, d, startH, startM, 0, 0, s.location)
	endToday := time.Date(y, m, d, endH, endM, 0, 0, s.location)

	// Overnight window (e.g., 23:00 → 07:00).
	// Two cases: we're in the late-night portion (after 23:00 today)
	// or the early-morning portion (before 07:00 today).
	if startToday.After(endToday) || startToday.Equal(endToday) {
		if now.After(startToday) || now.Equal(startToday) {
			// After 23:00 today → quiet ends at 07:00 tomorrow
			endTomorrow := endToday.Add(24 * time.Hour)
			return true, endTomorrow
		}
		if now.Before(endToday) {
			// Before 07:00 today → quiet ends at 07:00 today
			return true, endToday
		}
		return false, time.Time{}
	}

	// Same-day window (e.g., 13:00 → 15:00).
	if (now.After(startToday) || now.Equal(startToday)) && now.Before(endToday) {
		return true, endToday
	}
	return false, time.Time{}
}

// parseHourMinute extracts hour and minute from a "HH:MM" string.
func parseHourMinute(s string) (int, int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, 0, err
	}
	return t.Hour(), t.Minute(), nil
}

// ensureDefaults creates system default tasks if they don't already
// exist. Each default is idempotent — it checks by name + created_by
// before inserting. This runs once at startup.
//
// The defaults come from config flags (morning_briefing: true, etc.)
// and are translated into DefaultTask structs in cmd/run.go. The
// scheduler doesn't know about config — it just gets a list of tasks
// to ensure exist.
func (s *Scheduler) ensureDefaults() {
	if len(s.opts.Defaults) == 0 {
		return
	}

	for _, d := range s.opts.Defaults {
		// Check if this default already exists.
		existing, err := s.store.GetTaskByName(d.Name, "system")
		if err != nil {
			log.Error("checking for default task", "name", d.Name, "err", err)
			continue
		}
		if existing != nil {
			// Already exists — don't recreate. The user may have
			// customized the schedule or payload.
			continue
		}

		// Validate the cron expression.
		if err := ValidateCron(d.CronExpr); err != nil {
			log.Error("invalid cron in default task", "name", d.Name, "err", err)
			continue
		}

		// Compute the initial next_run.
		nextRun, err := NextRun(d.CronExpr, time.Now(), s.location)
		if err != nil {
			log.Error("computing next run for default task", "name", d.Name, "err", err)
			continue
		}

		priority := d.Priority
		if priority == "" {
			priority = "normal"
		}

		task := &memory.ScheduledTask{
			Name:         &d.Name,
			ScheduleType: "recurring",
			CronExpr:     &d.CronExpr,
			TaskType:     d.TaskType,
			Payload:      d.Payload,
			Enabled:      true,
			NextRun:      &nextRun,
			Priority:     priority,
			CreatedBy:    "system",
		}

		id, err := s.store.CreateScheduledTask(task)
		if err != nil {
			log.Error("creating default task", "name", d.Name, "err", err)
			continue
		}

		log.Info("created default task",
			"id", id, "name", d.Name, "cron", d.CronExpr,
			"type", d.TaskType, "priority", priority,
			"next_run", nextRun.In(s.location))
	}
}

// executeTask runs a single scheduled task based on its type, then
// computes the next run time for recurring tasks.
func (s *Scheduler) executeTask(task memory.ScheduledTask) {
	name := "<unnamed>"
	if task.Name != nil {
		name = *task.Name
	}

	log.Info("executing task", "id", task.ID, "name", name, "type", task.TaskType)

	switch task.TaskType {
	case "send_message":
		s.executeSendMessage(task)
	case "run_prompt":
		s.executeRunPrompt(task)
	case "mood_checkin":
		s.executeMoodCheckin(task)
	case "medication_checkin":
		s.executeMedicationCheckin(task)
	default:
		// Unknown task type — log and skip. Don't disable it in case
		// it's a future type that'll be supported after an upgrade
		// (e.g., mood_checkin, medication_checkin — coming with inline keyboards).
		log.Warn("unknown task type, skipping", "type", task.TaskType, "id", task.ID)
		return
	}

	// After successful execution, compute the next run time.
	//
	// - One-shot tasks ("once"): nextRun stays nil. MarkTaskRun will
	//   auto-disable it once run_count reaches max_runs.
	//
	// - Recurring/conditional tasks: parse the cron expression and
	//   find the next fire time. This is pre-computed and stored in
	//   the next_run column so the polling query stays efficient
	//   (just WHERE next_run <= now, using the index).
	var nextRun *time.Time

	if (task.ScheduleType == "recurring" || task.ScheduleType == "conditional") &&
		task.CronExpr != nil && *task.CronExpr != "" {
		now := time.Now().In(s.location)
		next, err := NextRun(*task.CronExpr, now, s.location)
		if err != nil {
			log.Error("computing next run from cron expression",
				"id", task.ID, "cron", *task.CronExpr, "err", err)
		} else {
			nextRun = &next
			log.Info("next run computed", "id", task.ID, "next_run", next.In(s.location))
		}
	}

	if err := s.store.MarkTaskRun(task.ID, nextRun); err != nil {
		log.Error("marking task run", "id", task.ID, "err", err)
	}
}

// recoverMissedTasks handles tasks that were due while the bot was down.
// One-shot tasks execute immediately (the user asked for a specific time,
// better late than never). Recurring tasks just skip to their next future
// run — we don't backfill missed executions since that would spam the user.
func (s *Scheduler) recoverMissedTasks() {
	now := time.Now().In(s.location)

	tasks, err := s.store.GetDueTasks(now)
	if err != nil {
		log.Error("recovering missed tasks", "err", err)
		return
	}

	if len(tasks) == 0 {
		return
	}

	log.Info("recovering missed tasks", "count", len(tasks))

	for _, task := range tasks {
		if task.ScheduleType == "once" {
			// One-shot: execute it now (late delivery is better than none).
			s.executeTask(task)
		} else {
			// Recurring/conditional: skip to the next future run.
			// Don't backfill — nobody wants 8 missed mood check-ins
			// delivered all at once when the bot restarts.
			if task.CronExpr != nil && *task.CronExpr != "" {
				now := time.Now().In(s.location)
				next, err := NextRun(*task.CronExpr, now, s.location)
				if err != nil {
					log.Error("recovery: bad cron expression",
						"id", task.ID, "cron", *task.CronExpr, "err", err)
					continue
				}
				if err := s.store.MarkTaskRun(task.ID, &next); err != nil {
					log.Error("recovery: updating next_run", "id", task.ID, "err", err)
					continue
				}

				name := "<unnamed>"
				if task.Name != nil {
					name = *task.Name
				}
				log.Info("recovered recurring task — skipped to next run",
					"id", task.ID, "name", name, "next_run", next.In(s.location))
			}
		}
	}
}

// Package scheduler runs scheduled tasks — reminders, cron jobs, and
// proactive messages. It's a goroutine-based ticker loop that polls
// the database every minute for tasks whose next_run time has passed.
//
// v0.2 supports only one-shot reminders (schedule_type="once",
// task_type="send_message"). The full cron system with recurring
// and conditional tasks lands in v0.6.
//
// Design philosophy: the scheduler is a "dumb executor with a smart
// payload." It wakes up, finds due tasks, executes them by type, and
// computes the next run time. All intelligence lives in task payloads.
package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
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

// Scheduler polls the database for due tasks and executes them.
// It runs in its own goroutine and communicates via context cancellation.
type Scheduler struct {
	store    *memory.Store
	sendFn   SendFunc
	location *time.Location // timezone for cron evaluation
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// New creates a scheduler. Call Start() to begin the polling loop.
//
// timezone is an IANA location string like "America/New_York".
// If empty or invalid, falls back to UTC. The timezone matters because
// when someone says "remind me at 3pm," we need to know WHICH 3pm.
func New(store *memory.Store, sendFn SendFunc, timezone string) *Scheduler {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		log.Warn("invalid timezone, falling back to UTC", "timezone", timezone, "err", err)
		loc = time.UTC
	}

	return &Scheduler{
		store:    store,
		sendFn:   sendFn,
		location: loc,
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
	ticker := time.NewTicker(1 * time.Minute)
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

// tick runs one polling cycle: find all due tasks and execute them.
func (s *Scheduler) tick() {
	now := time.Now().In(s.location)

	tasks, err := s.store.GetDueTasks(now)
	if err != nil {
		log.Error("polling for due tasks", "err", err)
		return
	}

	for _, task := range tasks {
		s.executeTask(task)
	}
}

// executeTask runs a single scheduled task based on its type.
// v0.2 only handles "send_message". v0.6 adds "run_prompt",
// "mood_checkin", "medication_checkin", "run_extraction", "run_journal".
func (s *Scheduler) executeTask(task memory.ScheduledTask) {
	name := "<unnamed>"
	if task.Name != nil {
		name = *task.Name
	}

	log.Info("executing task", "id", task.ID, "name", name, "type", task.TaskType)

	switch task.TaskType {
	case "send_message":
		s.executeSendMessage(task)
	default:
		// Unknown task type — log and skip. Don't disable it in case
		// it's a v0.6 type that'll be supported after an upgrade.
		log.Warn("unknown task type, skipping", "type", task.TaskType, "id", task.ID)
		return
	}

	// After successful execution, mark the task as run.
	// For one-shot tasks (max_runs=1), next_run is nil — MarkTaskRun
	// will auto-disable it when run_count reaches max_runs.
	// For recurring tasks (v0.6), we'd compute the next run from cron_expr here.
	var nextRun *time.Time // nil for one-shots
	if err := s.store.MarkTaskRun(task.ID, nextRun); err != nil {
		log.Error("marking task run", "id", task.ID, "err", err)
	}
}

// executeSendMessage handles the "send_message" task type.
// The payload is a JSON object with a "message" field.
func (s *Scheduler) executeSendMessage(task memory.ScheduledTask) {
	// Parse the payload to extract the message text.
	// json.RawMessage is already a []byte of JSON — we just need to
	// unmarshal it into a Go map. Like json.loads() in Python.
	var payload struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(task.Payload, &payload); err != nil {
		log.Error("parsing send_message payload", "id", task.ID, "err", err)
		return
	}

	if payload.Message == "" {
		log.Warn("send_message task has empty message", "id", task.ID)
		return
	}

	// Format the reminder message with a label.
	name := "Reminder"
	if task.Name != nil && *task.Name != "" {
		name = *task.Name
	}
	text := fmt.Sprintf("⏰ %s\n\n%s", name, payload.Message)

	if err := s.sendFn(text); err != nil {
		log.Error("sending scheduled message", "id", task.ID, "err", err)
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
			// Recurring/conditional (v0.6): skip to next future run.
			// For now, just disable — v0.6 will compute next_run from cron_expr.
			log.Info("skipping missed recurring task", "id", task.ID)
		}
	}
}

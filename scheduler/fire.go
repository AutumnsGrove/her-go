package scheduler

import (
	"context"
	"encoding/json"
	"fmt"

	"her/memory"
)

// FireResult reports the outcome of a forced task execution.
type FireResult struct {
	TaskID int64
	Kind   string
	Name   string
	Err    error
}

// withTaskContext sets deps.TaskContext for the duration of fn, mirroring
// what Scheduler.dispatch does on the normal cron-tick path. The forced-fire
// helpers below (FireTask, FireTaskByKind, FireAllUserTasks) bypass dispatch
// entirely, so without this, handlers that read deps.TaskContext.ID (e.g.
// send_message tagging its saved message with the firing schedule's ID)
// would silently see a nil TaskContext and lose that ID.
func withTaskContext(deps *Deps, tc *TaskContext, fn func() error) error {
	deps.TaskContext = tc
	defer func() { deps.TaskContext = nil }()
	return fn()
}

// FireTask dispatches a scheduler task by ID regardless of its next_fire time.
// Used for testing and manual triggering (sim, CLI). Does NOT update next_fire
// or attempt counts — this is a one-off forced execution, not a cron tick.
func FireTask(ctx context.Context, taskID int64, store memory.Store, deps *Deps) error {
	task, err := store.GetSchedulerTaskByID(taskID)
	if err != nil {
		return fmt.Errorf("fire task %d: %w", taskID, err)
	}
	if task == nil {
		return fmt.Errorf("fire task %d: not found", taskID)
	}

	h := lookup(task.Kind)
	if h == nil {
		return fmt.Errorf("fire task %d: no handler registered for kind %q", taskID, task.Kind)
	}

	name := task.Name
	if name == "" {
		name = "<unnamed>"
	}
	return withTaskContext(deps, &TaskContext{ID: task.ID, Name: name, Kind: task.Kind}, func() error {
		return runHandler(ctx, h, task.Payload, deps)
	})
}

// FireTaskByKind dispatches a task by handler kind with an explicit
// payload. Used for firing system tasks or testing handlers directly.
// There's no task row here (system tasks aren't necessarily persisted with
// an ID the caller has), so TaskContext gets a zero ID.
func FireTaskByKind(ctx context.Context, kind string, payload json.RawMessage, deps *Deps) error {
	h := lookup(kind)
	if h == nil {
		return fmt.Errorf("fire task: no handler registered for kind %q", kind)
	}
	if payload == nil {
		payload = json.RawMessage("{}")
	}
	return withTaskContext(deps, &TaskContext{Kind: kind}, func() error {
		return runHandler(ctx, h, payload, deps)
	})
}

// FireAllUserTasks dispatches every enabled named task. Used by the sim to
// exercise the full handler pipeline after schedule tools have created rows.
// Returns one result per task.
func FireAllUserTasks(ctx context.Context, store memory.Store, deps *Deps) []FireResult {
	tasks, err := store.ListManagedSchedulerTasks(false)
	if err != nil {
		return []FireResult{{Err: fmt.Errorf("listing managed tasks: %w", err)}}
	}

	var results []FireResult
	for _, t := range tasks {
		h := lookup(t.Kind)
		if h == nil {
			results = append(results, FireResult{
				TaskID: t.ID,
				Kind:   t.Kind,
				Name:   t.Name,
				Err:    fmt.Errorf("no handler for kind %q", t.Kind),
			})
			continue
		}

		name := t.Name
		if name == "" {
			name = "<unnamed>"
		}
		err := withTaskContext(deps, &TaskContext{ID: t.ID, Name: name, Kind: t.Kind}, func() error {
			return runHandler(ctx, h, t.Payload, deps)
		})
		results = append(results, FireResult{
			TaskID: t.ID,
			Kind:   t.Kind,
			Name:   t.Name,
			Err:    err,
		})
	}
	return results
}

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

	return runHandler(ctx, h, task.Payload, deps)
}

// FireTaskByKind dispatches a task by handler kind with an explicit
// payload. Used for firing system tasks or testing handlers directly.
func FireTaskByKind(ctx context.Context, kind string, payload json.RawMessage, deps *Deps) error {
	h := lookup(kind)
	if h == nil {
		return fmt.Errorf("fire task: no handler registered for kind %q", kind)
	}
	if payload == nil {
		payload = json.RawMessage("{}")
	}
	return runHandler(ctx, h, payload, deps)
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

		err := runHandler(ctx, h, t.Payload, deps)
		results = append(results, FireResult{
			TaskID: t.ID,
			Kind:   t.Kind,
			Name:   t.Name,
			Err:    err,
		})
	}
	return results
}

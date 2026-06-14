// Package send_task implements the send_task tool — delegates work to
// background agents. Supports two targets:
//
//   - "memory" (default): writes to the inter-agent inbox for the memory
//     agent to pick up during its post-reply run.
//   - "worker": fires the worker agent in a background goroutine to
//     produce a report, research document, or other file artifact.
//
// Both are fire-and-forget from the driver's perspective. The memory agent
// can notify back via notify_agent; the worker agent emits an event via
// the event bus when it finishes.
package send_task

import (
	"encoding/json"
	"fmt"

	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/send_task")

func init() {
	tools.Register("send_task", Handle)
}

// Handle routes the task to the appropriate target agent.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		Target    string  `json:"target"`
		TaskType  string  `json:"task_type"`
		Note      string  `json:"note"`
		Wait      bool    `json:"wait"`
		MemoryIDs []int64 `json:"memory_ids"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	target := args.Target
	if target == "" {
		target = "memory"
	}

	switch target {
	case "memory":
		return handleMemory(args.TaskType, args.Note, args.MemoryIDs, ctx)
	case "worker":
		return handleWorker(args.TaskType, args.Note, args.Wait, ctx)
	default:
		return fmt.Sprintf("error: unknown target %q (expected 'memory' or 'worker')", target)
	}
}

// handleMemory writes a task to the inbox for the memory agent.
func handleMemory(taskType, note string, memoryIDs []int64, ctx *tools.Context) string {
	payload, err := json.Marshal(map[string]interface{}{
		"task_type":  taskType,
		"note":       note,
		"memory_ids": memoryIDs,
	})
	if err != nil {
		return fmt.Sprintf("error encoding payload: %v", err)
	}

	id, err := ctx.Store.SendInbox("main", "memory", taskType, string(payload))
	if err != nil {
		return fmt.Sprintf("error sending task to inbox: %v", err)
	}

	log.Infof("  send_task: queued %s task (inbox ID=%d, %d memory IDs)", taskType, id, len(memoryIDs))
	return fmt.Sprintf("queued %s task for memory agent (inbox #%d)", taskType, id)
}

// handleWorker dispatches to the worker agent. When wait=true, runs
// synchronously and returns the worker's summary as the tool result.
// When wait=false (default), fires in a background goroutine.
func handleWorker(taskType, note string, wait bool, ctx *tools.Context) string {
	if taskType == "" {
		return "error: task_type is required for worker tasks"
	}
	if note == "" {
		return "error: note is required for worker tasks (describe what to research/build)"
	}

	// Synchronous mode: block until the worker finishes, return its summary.
	if wait {
		if ctx.WorkerCallbackSync == nil {
			return "error: synchronous worker dispatch not configured"
		}
		log.Infof("  send_task: running %s task synchronously (wait=true)", taskType)
		summary := ctx.WorkerCallbackSync(taskType, note)
		log.Infof("  send_task: %s task complete", taskType)
		return summary
	}

	// Async mode: fire-and-forget (existing behavior).
	if ctx.WorkerCallback == nil {
		return "error: worker agent not configured"
	}
	ctx.WorkerCallback(taskType, note)
	log.Infof("  send_task: dispatched %s task to worker agent", taskType)
	return fmt.Sprintf("dispatched %s task to worker agent — it will run in the background and notify you when done", taskType)
}

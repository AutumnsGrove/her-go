// Package send_task implements the send_task tool — delegates memory work to
// the background memory agent via the inter-agent inbox.
//
// The main agent uses this after doing research (recall_memories) to package
// up instructions for the memory agent. The memory agent picks up inbox
// messages automatically when it starts its post-reply run.
//
// This is a one-way handoff: the main agent fires and forgets. The memory
// agent can optionally send results back via notify_agent, which triggers
// a follow-up message to the user.
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

// Handle writes a task to the inbox for the memory agent to pick up.
// The payload is stored as JSON so the memory agent can parse it when
// it consumes the message.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		TaskType  string  `json:"task_type"`
		Note      string  `json:"note"`
		MemoryIDs []int64 `json:"memory_ids"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	// Re-encode the full args as the inbox payload so the memory agent
	// gets everything: task type, note, and memory IDs.
	payload, err := json.Marshal(args)
	if err != nil {
		return fmt.Sprintf("error encoding payload: %v", err)
	}

	id, err := ctx.Store.SendInbox("main", "memory", args.TaskType, string(payload))
	if err != nil {
		return fmt.Sprintf("error sending task to inbox: %v", err)
	}

	log.Infof("  send_task: queued %s task (inbox ID=%d, %d memory IDs)", args.TaskType, id, len(args.MemoryIDs))
	return fmt.Sprintf("queued %s task for memory agent (inbox #%d)", args.TaskType, id)
}

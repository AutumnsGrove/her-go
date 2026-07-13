package layers

import (
	"fmt"
	"strings"
	"time"

	"her/logger"
)

func init() {
	Register(PromptLayer{
		Name:    "Schedule Context",
		Order:   105, // after time (100), before tools (200)
		Stream:  StreamAgent,
		Builder: buildScheduleContext,
	})
}

// buildScheduleContext checks if the user is responding shortly after a
// scheduled message/prompt fired. If so, injects the schedule ID and name
// into the context so the agent knows which schedule is being referenced
// when the user says "delete this reminder" or "remove that schedule".
//
// This solves the UX problem where the bot sent a scheduled message but
// doesn't remember it came from a schedule when the user replies.
func buildScheduleContext(ctx *LayerContext) LayerResult {
	log := logger.WithPrefix("layers/schedule_context")
	log.Debug("schedule_context layer called")

	if ctx.Store == nil {
		log.Warn("schedule_context: ctx.Store is nil, skipping")
		return LayerResult{}
	}

	// Check the last 5 messages for any from the "scheduled" conversation.
	// If we find one within the last 10 minutes, assume the user might be
	// referring to that schedule.
	recent, err := ctx.Store.RecentMessages(ctx.ConversationID, 5)
	if err != nil {
		log.Error("schedule_context: failed to get recent messages", "err", err)
		return LayerResult{}
	}
	if len(recent) == 0 {
		log.Debug("schedule_context: no recent messages")
		return LayerResult{}
	}
	log.Debug("schedule_context: checking messages", "count", len(recent))

	now := time.Now()
	var scheduleInfo string

	// Look for recent assistant messages that might have come from a schedule.
	// We check the timestamp to see if it's within the "active context window"
	// where the user would naturally say "this reminder" or "that schedule".
	for i := len(recent) - 1; i >= 0; i-- {
		msg := recent[i]
		if msg.Role != "assistant" {
			continue
		}

		age := now.Sub(msg.Timestamp)
		if age > 10*time.Minute {
			break // too old to be relevant
		}

		// Check if this message contains our schedule marker.
		// send_message appends: "📅 Scheduled reminder #<ID>"
		// send_prompt injects: "[context: This message was triggered by schedule #<ID>"
		content := msg.ContentRaw
		if strings.Contains(content, "📅 Scheduled reminder #") ||
			strings.Contains(content, "triggered by schedule #") {

			// Extract the schedule ID from the message.
			// This is hacky but works for now - proper solution would be
			// to store schedule_id as a message column.
			var schedID int64
			var schedName string

			// Try both formats
			if _, err := fmt.Sscanf(content, "%*[^#]#%d", &schedID); err == nil && schedID > 0 {
				// Found an ID - try to get the full schedule info
				if task, err := ctx.Store.GetSchedulerTaskByID(schedID); err == nil && task != nil {
					schedName = task.Name
					if schedName == "" {
						schedName = task.Kind
					}
					scheduleInfo = fmt.Sprintf(
						"**Active schedule context:** The most recent message was triggered by "+
						"schedule #%d (%q, type: %s). If the user refers to \"this reminder\", "+
						"\"that schedule\", or asks to delete/remove/cancel it, they mean "+
						"schedule #%d. Use `delete_schedule` with task_id=%d.",
						schedID, schedName, task.Kind, schedID, schedID,
					)
					break
				}
			}
		}
	}

	if scheduleInfo == "" {
		log.Debug("schedule_context: no schedule markers found in recent messages")
		return LayerResult{}
	}

	log.Info("schedule_context: injecting schedule context", "info", scheduleInfo)
	return LayerResult{
		Content: scheduleInfo + "\n\n",
		Detail:  "schedule context",
	}
}

// Package notify_agent implements the notify_agent tool — sends results from
// a background agent back to the driver agent and triggers a follow-up message.
//
// When the memory agent finishes inbox tasks (cleanup, splits), it calls
// notify_agent instead of done. This does three things:
//   1. Writes the result to the inbox (so the driver agent can read details)
//   2. Fires an AgentEvent to wake up the driver agent
//   3. Ends the memory agent's turn (sets DoneCalled = true)
//
// The driver agent then reads the inbox and sends a brief follow-up to the user.
// If direct_message is set, the follow-up skips the agent loop entirely and
// sends the text directly — useful for simple confirmations.
package notify_agent

import (
	"encoding/json"
	"fmt"

	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/notify_agent")

func init() {
	tools.Register("notify_agent", Handle)
}

// Handle writes results to the inbox and fires an event to wake up the driver agent.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		Summary       string `json:"summary"`
		DirectMessage string `json:"direct_message"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	// Write the result to the inbox so the driver agent can read details.
	payload, err := json.Marshal(args)
	if err != nil {
		log.Error("failed to marshal notify_agent payload", "err", err)
		payload = []byte(`{}`)
	}
	_, err = ctx.Store.SendInbox("memory", "main", "result", string(payload))
	if err != nil {
		log.Error("failed to write inbox result", "err", err)
		// Don't fail the tool — the event still fires.
	}

	// Fire the agent event to wake up the driver agent. The bot layer
	// translates this into an agent.AgentEvent on the event channel.
	if ctx.AgentEventCB != nil {
		ctx.AgentEventCB(args.Summary, args.DirectMessage)
		log.Infof("  notify_agent: fired event (summary: %s)", args.Summary)
	} else {
		log.Warn("  notify_agent: no AgentEventCallback wired — event not fired")
	}

	// Notify implies done — the memory agent's turn is over.
	ctx.DoneCalled = true

	return fmt.Sprintf("notified driver agent: %s", args.Summary)
}

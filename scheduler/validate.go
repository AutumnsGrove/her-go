package scheduler

import (
	"encoding/json"
	"fmt"
)

// ValidatePayload checks that the payload JSON is well-formed and contains
// the required fields for the given task type. Returns nil if valid.
//
// Rules by type:
//   - send_message: payload must have a "message" field (non-empty payload required)
//   - send_prompt:  payload must have a "prompt" field (non-empty payload required)
//   - worker_briefing: if "depth" is present it must be "brief" or "deep"
func ValidatePayload(kind string, payload json.RawMessage) error {
	if len(payload) == 0 || string(payload) == "{}" || string(payload) == "null" {
		if kind == "send_message" {
			return fmt.Errorf("send_message requires payload with 'message' field")
		}
		if kind == "send_prompt" {
			return fmt.Errorf("send_prompt requires payload with 'prompt' field")
		}
		return nil
	}

	var p map[string]any
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("payload must be a JSON object")
	}

	switch kind {
	case "send_message":
		if _, ok := p["message"]; !ok {
			return fmt.Errorf("send_message payload requires 'message' field")
		}
	case "send_prompt":
		if _, ok := p["prompt"]; !ok {
			return fmt.Errorf("send_prompt payload requires 'prompt' field")
		}
	case "worker_briefing":
		if depth, ok := p["depth"]; ok {
			if d, ok := depth.(string); ok && d != "brief" && d != "deep" {
				return fmt.Errorf("worker_briefing depth must be 'brief' or 'deep'")
			}
		}
		if detail, ok := p["detail"]; ok {
			if d, ok := detail.(string); ok && d != "brief" && d != "default" && d != "detailed" {
				return fmt.Errorf("worker_briefing detail must be 'brief', 'default', or 'detailed'")
			}
		}
	}
	return nil
}

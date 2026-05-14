// Package create_card implements the create_card tool — creates a new
// organic (unprotected) memory topic card. Used by the memory agent as
// an escape hatch when no existing card fits, and by the dream cycle
// when splitting an overgrown card.
package create_card

import (
	"encoding/json"
	"fmt"

	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/create_card")

func init() {
	tools.Register("create_card", Handle)
}

type args struct {
	TopicSlug string `json:"topic_slug"`
	Name      string `json:"name"`
	Subject   string `json:"subject"`
}

// Handle creates a new organic memory card with an empty summary.
// The dream cycle will populate the summary on its next run.
func Handle(argsJSON string, ctx *tools.Context) string {
	var a args
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "error: invalid arguments"
	}
	if a.TopicSlug == "" || a.Name == "" || a.Subject == "" {
		return "error: topic_slug, name, and subject are all required"
	}
	if a.Subject != "user" && a.Subject != "self" {
		return "error: subject must be 'user' or 'self'"
	}

	// Check for slug collision.
	existing, err := ctx.Store.GetCard(a.TopicSlug)
	if err != nil {
		log.Error("create_card lookup failed", "err", err)
		return fmt.Sprintf("error: %v", err)
	}
	if existing != nil {
		return fmt.Sprintf("error: a card with slug %q already exists", a.TopicSlug)
	}

	card, err := ctx.Store.CreateCard(a.TopicSlug, a.Name, a.Subject, 0)
	if err != nil {
		log.Error("create_card failed", "err", err)
		return fmt.Sprintf("error: %v", err)
	}

	log.Infof("created card %s (%s): %s", a.TopicSlug, a.Subject, a.Name)
	return fmt.Sprintf("Created card %q (%s) — %s", card.TopicSlug, card.Subject, card.Name)
}

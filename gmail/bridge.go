// Package gmail provides read-only access to a Gmail account via the
// Google Gmail API. It follows the Bridge interface pattern used by
// calendar/ — a production implementation (APIBridge) hits the real API,
// and a fake implementation (FakeBridge) runs in-memory for sims.
//
// The package never stores emails locally. Every search and read goes
// through the Gmail API (or the fake). Emails live on Google's servers.
package gmail

import (
	"context"
	"time"
)

// Bridge is the contract for email access. Production uses the Gmail API;
// sims use a FakeBridge seeded from YAML. Tools call this interface —
// they never know which implementation is behind it.
type Bridge interface {
	// Search returns email summaries matching a Gmail search query.
	// An empty query returns recent messages. Page is 1-indexed.
	Search(ctx context.Context, query string, page int) (SearchResult, error)

	// Read returns the full content of a single email by message ID.
	Read(ctx context.Context, id string) (*Message, error)
}

// SearchResult wraps a page of email summaries with pagination info.
type SearchResult struct {
	Messages []MessageSummary
	HasMore  bool
}

// MessageSummary is the compact view returned by Search — enough for
// the agent to decide which emails are worth reading in full.
type MessageSummary struct {
	ID      string    `json:"id"`
	From    string    `json:"from"`
	To      string    `json:"to"`
	Subject string    `json:"subject"`
	Snippet string    `json:"snippet"`
	Date    time.Time `json:"date"`
	Unread  bool      `json:"unread"`
}

// Message is the full view of a single email, returned by Read.
// Body is extracted as plain text (text/plain preferred, HTML stripped
// as fallback). Attachments are listed by name but not downloaded.
type Message struct {
	MessageSummary
	Body        string   `json:"body"`
	Attachments []string `json:"attachments"`
}

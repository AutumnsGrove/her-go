package gmail

import (
	"fmt"
	"sort"
	"strings"
)

// FakeBridge is an in-memory email store for sims. Seeded from YAML via
// the Seed method, it supports basic search by substring matching on
// from/subject/body fields, plus a subset of Gmail search operators
// (from:, subject:, is:unread). No network calls, no Google dependency.
type FakeBridge struct {
	messages []Message
	pageSize int
}

// NewFakeBridge creates an empty FakeBridge with the given page size.
// Pass 0 for the default (20 messages per page).
func NewFakeBridge(pageSize int) *FakeBridge {
	if pageSize <= 0 {
		pageSize = 20
	}
	return &FakeBridge{pageSize: pageSize}
}

// Seed populates the fake inbox. Messages are stored in the order given
// and sorted by date descending (newest first) for consistency with
// how Gmail returns results.
func (f *FakeBridge) Seed(msgs []Message) {
	f.messages = make([]Message, len(msgs))
	copy(f.messages, msgs)
	sort.Slice(f.messages, func(i, j int) bool {
		return f.messages[i].Date.After(f.messages[j].Date)
	})
}

// Search filters messages by query. Supports a simplified subset of
// Gmail search syntax:
//   - from:X     — matches if From contains X (case-insensitive)
//   - subject:X  — matches if Subject contains X (case-insensitive)
//   - is:unread  — matches only unread messages
//   - bare words — matches if From, Subject, or Body contains the word
//
// Multiple terms are AND-ed together. Page is 1-indexed.
func (f *FakeBridge) Search(query string, page int) (SearchResult, error) {
	if page < 1 {
		page = 1
	}

	filtered := f.filter(query)

	start := (page - 1) * f.pageSize
	if start >= len(filtered) {
		return SearchResult{HasMore: false}, nil
	}

	end := start + f.pageSize
	hasMore := end < len(filtered)
	if end > len(filtered) {
		end = len(filtered)
	}

	summaries := make([]MessageSummary, end-start)
	for i, msg := range filtered[start:end] {
		summaries[i] = msg.MessageSummary
	}

	return SearchResult{
		Messages: summaries,
		HasMore:  hasMore,
	}, nil
}

// Read returns the full message by ID. Returns an error if not found.
func (f *FakeBridge) Read(id string) (*Message, error) {
	for _, msg := range f.messages {
		if msg.ID == id {
			cp := msg
			return &cp, nil
		}
	}
	return nil, fmt.Errorf("message %q not found", id)
}

// filter applies a simplified Gmail search query to the message list.
func (f *FakeBridge) filter(query string) []Message {
	if query == "" {
		return f.messages
	}

	terms := parseQuery(query)
	var results []Message

	for _, msg := range f.messages {
		if matchesAll(msg, terms) {
			results = append(results, msg)
		}
	}
	return results
}

type queryTerm struct {
	operator string // "from", "subject", "is", or "" for bare words
	value    string
}

// parseQuery splits a Gmail-style query into structured terms.
// Handles from:X, subject:X, is:unread, and bare words.
func parseQuery(query string) []queryTerm {
	var terms []queryTerm
	for _, token := range strings.Fields(query) {
		lower := strings.ToLower(token)
		if strings.HasPrefix(lower, "from:") {
			terms = append(terms, queryTerm{"from", lower[5:]})
		} else if strings.HasPrefix(lower, "subject:") {
			terms = append(terms, queryTerm{"subject", lower[8:]})
		} else if lower == "is:unread" {
			terms = append(terms, queryTerm{"is", "unread"})
		} else if lower == "is:read" {
			terms = append(terms, queryTerm{"is", "read"})
		} else {
			terms = append(terms, queryTerm{"", lower})
		}
	}
	return terms
}

// matchesAll returns true if a message matches every query term.
func matchesAll(msg Message, terms []queryTerm) bool {
	for _, t := range terms {
		if !matchesTerm(msg, t) {
			return false
		}
	}
	return true
}

func matchesTerm(msg Message, t queryTerm) bool {
	switch t.operator {
	case "from":
		return strings.Contains(strings.ToLower(msg.From), t.value)
	case "subject":
		return strings.Contains(strings.ToLower(msg.Subject), t.value)
	case "is":
		if t.value == "unread" {
			return msg.Unread
		}
		if t.value == "read" {
			return !msg.Unread
		}
		return true
	default:
		// Bare word: match against from, subject, snippet, or body
		lower := t.value
		return strings.Contains(strings.ToLower(msg.From), lower) ||
			strings.Contains(strings.ToLower(msg.Subject), lower) ||
			strings.Contains(strings.ToLower(msg.Snippet), lower) ||
			strings.Contains(strings.ToLower(msg.Body), lower)
	}
}

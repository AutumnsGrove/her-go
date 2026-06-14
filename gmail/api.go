package gmail

import (
	"context"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"her/config"
	"her/logger"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	gm "google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

var log = logger.WithPrefix("gmail")

// APIBridge is the production implementation of Bridge. It talks to the
// real Gmail API using OAuth2 credentials from config.yaml. The refresh
// token is long-lived — the oauth2 package handles access token renewal
// automatically. No browser flow needed at runtime.
type APIBridge struct {
	svc      *gm.Service
	pageSize int64
}

// NewAPIBridge creates a Gmail API client from config. Returns an error
// if credentials are missing or the API client can't initialize.
//
// Under the hood, oauth2.NewClient wraps an http.Client that automatically
// refreshes the access token when it expires. This is similar to how
// requests.Session works in Python with an auth plugin, except Go's
// oauth2 package bakes the token lifecycle into the HTTP transport layer.
func NewAPIBridge(cfg *config.GmailConfig) (*APIBridge, error) {
	if cfg.ClientID == "" || cfg.ClientSecret == "" || cfg.RefreshToken == "" {
		return nil, fmt.Errorf("gmail: client_id, client_secret, and refresh_token are all required")
	}

	// Build an OAuth2 config. We only need the token source — the
	// auth URL and redirect URL are unused since we already have a
	// refresh token from the OAuth Playground.
	oauthCfg := &oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		Endpoint:     google.Endpoint,
		Scopes:       []string{gm.GmailReadonlyScope},
	}

	// Create a token with just the refresh token. The oauth2 package
	// will automatically fetch a fresh access token on first use.
	token := &oauth2.Token{RefreshToken: cfg.RefreshToken}
	ctx := context.Background()
	client := oauthCfg.Client(ctx, token)

	svc, err := gm.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		return nil, fmt.Errorf("gmail: failed to create service: %w", err)
	}

	pageSize := int64(cfg.MaxResults)
	if pageSize <= 0 {
		pageSize = 20
	}

	log.Info("Gmail API bridge initialized", "account", cfg.Account)
	return &APIBridge{svc: svc, pageSize: pageSize}, nil
}

// Search queries Gmail and returns message summaries. Uses Gmail's native
// search syntax (from:, subject:, is:unread, before:, after:, label:, etc.).
// Page is 1-indexed; pagination uses Gmail's pageToken internally.
func (a *APIBridge) Search(query string, page int) (SearchResult, error) {
	if page < 1 {
		page = 1
	}

	// Gmail API uses page tokens, not page numbers. For page 1 we
	// send no token. For page N>1, we fetch N-1 pages of IDs to get
	// the right token. This is inefficient for deep pagination but
	// fine for our use case (agents rarely go past page 2-3).
	var pageToken string
	for i := 1; i < page; i++ {
		list, err := a.svc.Users.Messages.List("me").
			Q(query).
			MaxResults(a.pageSize).
			PageToken(pageToken).
			Do()
		if err != nil {
			return SearchResult{}, fmt.Errorf("gmail search: %w", err)
		}
		pageToken = list.NextPageToken
		if pageToken == "" {
			return SearchResult{HasMore: false}, nil
		}
	}

	// Fetch the actual page
	listCall := a.svc.Users.Messages.List("me").
		Q(query).
		MaxResults(a.pageSize)
	if pageToken != "" {
		listCall = listCall.PageToken(pageToken)
	}

	list, err := listCall.Do()
	if err != nil {
		return SearchResult{}, fmt.Errorf("gmail search: %w", err)
	}

	// Fetch metadata for each message (list only returns IDs)
	summaries := make([]MessageSummary, 0, len(list.Messages))
	for _, stub := range list.Messages {
		msg, err := a.svc.Users.Messages.Get("me", stub.Id).
			Format("metadata").
			MetadataHeaders("From", "To", "Subject", "Date").
			Do()
		if err != nil {
			log.Warn("Failed to fetch message metadata", "id", stub.Id, "error", err)
			continue
		}
		summaries = append(summaries, toSummary(msg))
	}

	return SearchResult{
		Messages: summaries,
		HasMore:  list.NextPageToken != "",
	}, nil
}

// Read fetches the full content of a single email by message ID.
// Returns headers, plain text body, and attachment filenames.
func (a *APIBridge) Read(id string) (*Message, error) {
	msg, err := a.svc.Users.Messages.Get("me", id).
		Format("full").
		Do()
	if err != nil {
		return nil, fmt.Errorf("gmail read %s: %w", id, err)
	}

	summary := toSummary(msg)
	body, attachments := extractBody(msg)

	return &Message{
		MessageSummary: summary,
		Body:           body,
		Attachments:    attachments,
	}, nil
}

// toSummary converts a Gmail API message to our MessageSummary type.
func toSummary(msg *gm.Message) MessageSummary {
	headers := headerMap(msg.Payload)

	s := MessageSummary{
		ID:      msg.Id,
		From:    formatFrom(headers["From"]),
		To:      headers["To"],
		Subject: headers["Subject"],
		Snippet: msg.Snippet,
		Unread:  hasLabel(msg.LabelIds, "UNREAD"),
	}

	if d, err := parseDate(headers["Date"]); err == nil {
		s.Date = d
	}

	return s
}

// headerMap extracts headers into a simple map. Gmail returns headers
// as a slice of {Name, Value} pairs.
func headerMap(part *gm.MessagePart) map[string]string {
	m := make(map[string]string)
	if part == nil {
		return m
	}
	for _, h := range part.Headers {
		m[h.Name] = h.Value
	}
	return m
}

// formatFrom cleans up the From header. Email From headers can be
// "Name <email>" or just "email" — we normalize to a readable form.
func formatFrom(from string) string {
	if from == "" {
		return ""
	}
	addr, err := mail.ParseAddress(from)
	if err != nil {
		return from
	}
	if addr.Name != "" {
		return fmt.Sprintf("%s <%s>", addr.Name, addr.Address)
	}
	return addr.Address
}

// hasLabel checks if a label ID is in the list. Gmail uses label IDs
// like "UNREAD", "INBOX", "SPAM" etc.
func hasLabel(labels []string, target string) bool {
	for _, l := range labels {
		if l == target {
			return true
		}
	}
	return false
}

// parseDate handles the variety of date formats found in email headers.
// RFC 2822 is the standard but real-world emails are creative.
func parseDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("empty date")
	}

	formats := []string{
		time.RFC1123Z,
		time.RFC1123,
		"Mon, 2 Jan 2006 15:04:05 -0700",
		"Mon, 2 Jan 2006 15:04:05 -0700 (MST)",
		"2 Jan 2006 15:04:05 -0700",
		time.RFC3339,
	}

	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unparseable date: %s", s)
}

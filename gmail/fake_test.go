package gmail

import (
	"context"
	"testing"
	"time"
)

func seedBridge() *FakeBridge {
	fb := NewFakeBridge(3) // small page for pagination tests
	fb.Seed([]Message{
		{
			MessageSummary: MessageSummary{
				ID: "msg-1", From: "Mom <mom@example.com>", Subject: "Dinner Sunday?",
				Snippet: "Hey sweetie, are you free", Date: time.Now().Add(-1 * time.Hour), Unread: true,
			},
			Body: "Hey sweetie, are you free for dinner this Sunday?",
		},
		{
			MessageSummary: MessageSummary{
				ID: "msg-2", From: "GitHub <noreply@github.com>", Subject: "[her-go] PR #87 merged",
				Snippet: "Your pull request has been merged", Date: time.Now().Add(-2 * time.Hour), Unread: false,
			},
			Body: "Pull request #87 has been merged into main.",
		},
		{
			MessageSummary: MessageSummary{
				ID: "msg-3", From: "Jess <jess@example.com>", Subject: "Re: coffee next week?",
				Snippet: "Tuesday works for me", Date: time.Now().Add(-24 * time.Hour), Unread: true,
			},
			Body: "Tuesday works for me! How about 3pm?",
		},
		{
			MessageSummary: MessageSummary{
				ID: "msg-4", From: "Spotify <no-reply@spotify.com>", Subject: "Your Weekly Discovery",
				Snippet: "30 songs picked just for you", Date: time.Now().Add(-48 * time.Hour), Unread: false,
			},
			Body: "30 songs picked just for you. Listen now.",
		},
		{
			MessageSummary: MessageSummary{
				ID: "msg-5", From: "Dad <dad@example.com>", Subject: "RE: Dinner Sunday?",
				Snippet: "I'll be grilling burgers", Date: time.Now().Add(-30 * time.Minute), Unread: true,
			},
			Body: "I'll be grilling burgers. See you at 6!",
		},
	})
	return fb
}

func TestFakeBridge_SearchEmpty(t *testing.T) {
	fb := seedBridge()
	ctx := context.Background()

	result, err := fb.Search(ctx, "", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Messages) != 3 {
		t.Errorf("page 1: got %d messages, want 3 (page size)", len(result.Messages))
	}
	if !result.HasMore {
		t.Error("expected HasMore=true with 5 messages and page size 3")
	}

	// Verify newest-first ordering (Seed sorts by date desc)
	if result.Messages[0].ID != "msg-5" {
		t.Errorf("expected newest message first, got %s", result.Messages[0].ID)
	}
}

func TestFakeBridge_SearchPagination(t *testing.T) {
	fb := seedBridge()
	ctx := context.Background()

	// Page 2
	result, err := fb.Search(ctx, "", 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Messages) != 2 {
		t.Errorf("page 2: got %d messages, want 2 (remainder)", len(result.Messages))
	}
	if result.HasMore {
		t.Error("expected HasMore=false on last page")
	}

	// Page 3 (out of range)
	result, err = fb.Search(ctx, "", 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Messages) != 0 {
		t.Errorf("page 3: got %d messages, want 0", len(result.Messages))
	}
}

func TestFakeBridge_SearchFrom(t *testing.T) {
	fb := seedBridge()
	ctx := context.Background()

	result, err := fb.Search(ctx, "from:mom", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Messages) != 1 {
		t.Errorf("from:mom: got %d, want 1", len(result.Messages))
	}
	if len(result.Messages) > 0 && result.Messages[0].ID != "msg-1" {
		t.Errorf("expected msg-1, got %s", result.Messages[0].ID)
	}
}

func TestFakeBridge_SearchSubject(t *testing.T) {
	fb := seedBridge()
	ctx := context.Background()

	result, err := fb.Search(ctx, "subject:coffee", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Messages) != 1 {
		t.Errorf("subject:coffee: got %d, want 1", len(result.Messages))
	}
}

func TestFakeBridge_SearchUnread(t *testing.T) {
	fb := seedBridge()
	ctx := context.Background()

	result, err := fb.Search(ctx, "is:unread", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 3 unread: msg-1, msg-3, msg-5
	if len(result.Messages) != 3 {
		t.Errorf("is:unread: got %d, want 3", len(result.Messages))
	}

	// is:read
	result, err = fb.Search(ctx, "is:read", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Messages) != 2 {
		t.Errorf("is:read: got %d, want 2", len(result.Messages))
	}
}

func TestFakeBridge_SearchBareWord(t *testing.T) {
	fb := seedBridge()
	ctx := context.Background()

	result, err := fb.Search(ctx, "burgers", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Messages) != 1 {
		t.Errorf("bare word 'burgers': got %d, want 1", len(result.Messages))
	}
}

func TestFakeBridge_SearchCombined(t *testing.T) {
	fb := seedBridge()
	ctx := context.Background()

	// from:example.com AND is:unread — should match msg-1, msg-3, msg-5
	result, err := fb.Search(ctx, "from:example.com is:unread", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Messages) != 3 {
		t.Errorf("combined: got %d, want 3", len(result.Messages))
	}
}

func TestFakeBridge_SearchNewerThan(t *testing.T) {
	fb := seedBridge()
	ctx := context.Background()

	// newer_than:2h — should match msg-1 (1h ago) and msg-5 (30m ago)
	result, err := fb.Search(ctx, "newer_than:2h", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Messages) != 2 {
		t.Errorf("newer_than:2h: got %d, want 2", len(result.Messages))
	}
}

func TestFakeBridge_Read(t *testing.T) {
	fb := seedBridge()
	ctx := context.Background()

	msg, err := fb.Read(ctx, "msg-3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.Subject != "Re: coffee next week?" {
		t.Errorf("subject: got %q, want %q", msg.Subject, "Re: coffee next week?")
	}
	if msg.Body != "Tuesday works for me! How about 3pm?" {
		t.Errorf("body mismatch: got %q", msg.Body)
	}
}

func TestFakeBridge_ReadNotFound(t *testing.T) {
	fb := seedBridge()
	ctx := context.Background()

	_, err := fb.Read(ctx, "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent message")
	}
}

func TestParseNewerThan(t *testing.T) {
	tests := []struct {
		input string
		want  time.Duration
	}{
		{"7d", 7 * 24 * time.Hour},
		{"1d", 24 * time.Hour},
		{"2h", 2 * time.Hour},
		{"30m", 30 * time.Minute},
		{"", 0},
		{"x", 0},
		{"7x", 0},
		{"abc", 0},
	}
	for _, tt := range tests {
		got := parseNewerThan(tt.input)
		if got != tt.want {
			t.Errorf("parseNewerThan(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

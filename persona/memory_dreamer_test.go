package persona

import (
	"strings"
	"testing"
	"time"

	"her/memory"
)

func TestBuildDreamerTranscript_CardsWithChildren(t *testing.T) {
	now := time.Now()
	cards := []memory.MemoryCard{
		{ID: 1, TopicSlug: "health", Name: "Health", Summary: "Physical and mental health", Subject: "user", Protected: true, UpdatedAt: now.Add(-24 * time.Hour), Version: 3},
		{ID: 2, TopicSlug: "my-identity", Name: "My Identity", Summary: "Who I am", Subject: "self", Protected: true, UpdatedAt: now.Add(-48 * time.Hour), Version: 2},
	}
	children := map[int64][]memory.Memory{
		1: {
			{ID: 10, Content: "Takes lurasidone", Category: "health", Subject: "user"},
			{ID: 11, Content: "Executive dysfunction", Category: "health", Subject: "user"},
		},
		2: {
			{ID: 20, Content: "Name Mira connects to ocean", Category: "identity", Subject: "self"},
		},
	}
	logEntries := []memory.MemoryLogEntry{
		{ID: 1, CardID: 1, Delta: "added medication info", Operation: "update", CreatedAt: now.Add(-1 * time.Hour)},
	}

	result := buildDreamerTranscript(cards, children, logEntries)

	if !strings.Contains(result, "[health]") {
		t.Error("missing health card slug")
	}
	if !strings.Contains(result, "2 memories") {
		t.Error("missing memory count for health card")
	}
	if !strings.Contains(result, "#10") {
		t.Error("missing child memory ID")
	}
	if !strings.Contains(result, "Takes lurasidone") {
		t.Error("missing child memory content")
	}
	if !strings.Contains(result, "[my-identity]") {
		t.Error("missing self card slug")
	}
	if !strings.Contains(result, "Recent Changes") {
		t.Error("missing changelog section")
	}
	if !strings.Contains(result, "added medication info") {
		t.Error("missing log entry delta")
	}
}

func TestBuildDreamerTranscript_EmptyCardsOmitted(t *testing.T) {
	cards := []memory.MemoryCard{
		{ID: 1, TopicSlug: "routines", Name: "Routines", Subject: "user", Protected: true, UpdatedAt: time.Now(), Version: 1},
	}
	children := map[int64][]memory.Memory{
		1: {},
	}

	result := buildDreamerTranscript(cards, children, nil)

	if strings.Contains(result, "[routines]") {
		t.Error("empty card should be omitted from transcript")
	}
	if strings.Contains(result, "Recent Changes") {
		t.Error("should have no changelog section with nil log entries")
	}
}

func TestBuildDreamerTranscript_SkipsEmptyCards(t *testing.T) {
	now := time.Now()
	cards := []memory.MemoryCard{
		{ID: 1, TopicSlug: "health", Name: "Health", Summary: "Has meds", Subject: "user", UpdatedAt: now, Version: 2},
		{ID: 2, TopicSlug: "patterns", Name: "Patterns", Summary: "", Subject: "user", UpdatedAt: now, Version: 1},
		{ID: 3, TopicSlug: "my-identity", Name: "My Identity", Summary: "Who I am", Subject: "self", UpdatedAt: now, Version: 2},
	}
	children := map[int64][]memory.Memory{
		1: {{ID: 10, Content: "Takes lurasidone"}},
		2: {}, // empty card
		3: {{ID: 20, Content: "Name means ocean"}},
	}

	result := buildDreamerTranscript(cards, children, nil)

	if !strings.Contains(result, "[health]") {
		t.Error("should include non-empty health card")
	}
	if !strings.Contains(result, "[my-identity]") {
		t.Error("should include non-empty self card")
	}
	if strings.Contains(result, "[patterns]") {
		t.Error("should skip empty patterns card")
	}
}

func TestBuildDreamerTranscript_ChangedCardsHeader(t *testing.T) {
	cards := []memory.MemoryCard{
		{ID: 1, TopicSlug: "health", Name: "Health", Summary: "Meds", Subject: "user", UpdatedAt: time.Now(), Version: 2},
	}
	children := map[int64][]memory.Memory{
		1: {{ID: 10, Content: "Takes lurasidone"}},
	}

	result := buildDreamerTranscript(cards, children, nil)

	if !strings.Contains(result, "changed cards only") {
		t.Error("transcript should mention changed cards only")
	}
	if !strings.Contains(result, "Unchanged cards are omitted") {
		t.Error("transcript should explain omission")
	}
}

func TestBuildDreamerTranscript_NoSummary(t *testing.T) {
	cards := []memory.MemoryCard{
		{ID: 1, TopicSlug: "patterns", Name: "Patterns", Summary: "", Subject: "user", UpdatedAt: time.Now(), Version: 1},
	}
	children := map[int64][]memory.Memory{
		1: {{ID: 10, Content: "Some pattern"}},
	}

	result := buildDreamerTranscript(cards, children, nil)

	if !strings.Contains(result, "(no summary yet)") {
		t.Error("empty summary should show placeholder")
	}
}

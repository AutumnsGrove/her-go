package agent

import (
	"strings"
	"testing"

	"her/memory"
)

func TestBuildIntrospectionTranscript_AllSections(t *testing.T) {
	input := IntrospectionAgentInput{
		UserMessage: "I had a rough day at work",
		ThinkTraces: []string{
			"Autumn is venting — I should validate first",
			"She mentioned work stress before, this is a pattern",
		},
		ReplyText: "That sounds really draining. What part hit you the hardest?",
		SelfMemories: []memory.Memory{
			{ID: 1, Content: "I tend to validate before problem-solving"},
			{ID: 2, Content: "I ask follow-up questions to show I'm listening"},
		},
		PersonaText: "I am Mira, a thoughtful companion.",
	}

	result := buildIntrospectionTranscript(input)

	// All five sections should be present.
	sections := []string{
		"## What the user said",
		"## What I said",
		"## How I arrived at this reply",
		"## What I already know about myself",
		"## My current self-image",
	}
	for _, s := range sections {
		if !strings.Contains(result, s) {
			t.Errorf("missing section: %s", s)
		}
	}

	// Content should appear in the right sections.
	if !strings.Contains(result, "rough day at work") {
		t.Error("user message not in transcript")
	}
	if !strings.Contains(result, "That sounds really draining") {
		t.Error("reply text not in transcript")
	}
	if !strings.Contains(result, "Autumn is venting") {
		t.Error("think trace not in transcript")
	}
	if !strings.Contains(result, "[ID=1]") {
		t.Error("self-memory ID not in transcript")
	}
	if !strings.Contains(result, "I am Mira") {
		t.Error("persona text not in transcript")
	}
}

func TestBuildIntrospectionTranscript_EmptyFields(t *testing.T) {
	input := IntrospectionAgentInput{
		UserMessage: "hello",
		ReplyText:   "hi there",
	}

	result := buildIntrospectionTranscript(input)

	if !strings.Contains(result, "No thinking traces for this turn.") {
		t.Error("should show 'no thinking traces' when empty")
	}
	if !strings.Contains(result, "No self-memories yet.") {
		t.Error("should show 'no self-memories' when empty")
	}
	if !strings.Contains(result, "No persona file configured.") {
		t.Error("should show 'no persona' when empty")
	}
}

func TestBuildIntrospectionTranscript_SelfMemoryFormat(t *testing.T) {
	input := IntrospectionAgentInput{
		UserMessage: "test",
		ReplyText:   "test",
		SelfMemories: []memory.Memory{
			{ID: 42, Content: "I use cooking metaphors for emotional advice"},
			{ID: 99, Content: "When Autumn vents, I validate before pivoting"},
		},
	}

	result := buildIntrospectionTranscript(input)

	if !strings.Contains(result, "- [ID=42] I use cooking metaphors") {
		t.Error("self-memory 42 not formatted correctly")
	}
	if !strings.Contains(result, "- [ID=99] When Autumn vents") {
		t.Error("self-memory 99 not formatted correctly")
	}
}

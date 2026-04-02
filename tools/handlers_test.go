// Package tools_test exercises tool handlers through the tools.Execute()
// dispatch path — the same code path the agent loop uses. Each blank import
// below triggers the tool's init() function, which calls tools.Register().
//
// This external test package (tools_test, not tools) ensures we're testing the
// public API. If a handler fails to register, Execute returns "unknown tool"
// and the test catches it immediately.
package tools_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"her/memory"
	"her/testutil"
	"her/tools"

	// Blank imports trigger handler registration via init().
	_ "her/tools/create_reminder"
	_ "her/tools/create_schedule"
	_ "her/tools/delete_schedule"
	_ "her/tools/done"
	_ "her/tools/get_current_time"
	_ "her/tools/list_schedules"
	_ "her/tools/no_action"
	_ "her/tools/recall_memories"
	_ "her/tools/remove_fact"
	_ "her/tools/save_fact"
	_ "her/tools/save_self_fact"
	_ "her/tools/search_history"
	_ "her/tools/think"
	_ "her/tools/update_fact"
	_ "her/tools/update_schedule"
	_ "her/tools/view_image"
)

// seedMessage inserts a dummy message into the store so handlers that
// reference TriggerMsgID via FK constraints can work. Returns the message ID.
func seedMessage(t *testing.T, ctx *tools.Context) int64 {
	t.Helper()
	id, err := ctx.Store.SaveMessage("user", "test message", "test message", ctx.ConversationID)
	if err != nil {
		t.Fatalf("seeding message: %v", err)
	}
	ctx.TriggerMsgID = id
	return id
}

// ===========================================================================
// Registry & Dispatch
// ===========================================================================

func TestExecute_UnknownTool(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	result := tools.Execute("nonexistent_tool", `{}`, ctx)
	if !strings.Contains(result, "unknown tool") {
		t.Errorf("expected 'unknown tool' error, got: %s", result)
	}
}

func TestExecute_MalformedJSON(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	result := tools.Execute("think", `{"thought": "incomple`, ctx)
	if !strings.Contains(result, "malformed JSON") {
		t.Errorf("expected malformed JSON error, got: %s", result)
	}
}

func TestExecute_EmptyArgsJSON(t *testing.T) {
	// Empty string is valid — some tools ignore args entirely.
	ctx := testutil.TestToolContext(t)
	result := tools.Execute("done", "", ctx)
	if !ctx.DoneCalled {
		t.Errorf("done should work with empty argsJSON, got: %s", result)
	}
}

func TestExecute_HasHandler(t *testing.T) {
	if !tools.HasHandler("think") {
		t.Error("think should be registered")
	}
	if tools.HasHandler("totally_fake_tool") {
		t.Error("fake tool should not be registered")
	}
}

// ===========================================================================
// think
// ===========================================================================

func TestThink_HappyPath(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	result := tools.Execute("think", `{"thought": "considering options"}`, ctx)
	if result != "tool call complete" {
		t.Errorf("got: %s", result)
	}
}

func TestThink_EmptyArgs(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	// Missing "thought" key — handler should not panic.
	result := tools.Execute("think", `{}`, ctx)
	if result != "tool call complete" {
		t.Errorf("got: %s", result)
	}
}

func TestThink_EmptyThought(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	result := tools.Execute("think", `{"thought": ""}`, ctx)
	if result != "tool call complete" {
		t.Errorf("got: %s", result)
	}
}

func TestThink_NoSideEffects(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	tools.Execute("think", `{"thought": "deep thought"}`, ctx)
	// Think should never touch context state.
	if ctx.DoneCalled {
		t.Error("think should not set DoneCalled")
	}
	if ctx.ReplyCalled {
		t.Error("think should not set ReplyCalled")
	}
	if len(ctx.SavedFacts) > 0 {
		t.Error("think should not save facts")
	}
}

// ===========================================================================
// done
// ===========================================================================

func TestDone_SetsDoneCalled(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	if ctx.DoneCalled {
		t.Fatal("DoneCalled should start false")
	}
	result := tools.Execute("done", `{}`, ctx)
	if !ctx.DoneCalled {
		t.Error("DoneCalled should be true after done")
	}
	if !strings.Contains(result, "turn complete") {
		t.Errorf("got: %s", result)
	}
}

func TestDone_Idempotent(t *testing.T) {
	// Calling done twice should not panic or produce different behavior.
	ctx := testutil.TestToolContext(t)
	tools.Execute("done", `{}`, ctx)
	result := tools.Execute("done", `{}`, ctx)
	if !ctx.DoneCalled {
		t.Error("should still be done")
	}
	if !strings.Contains(result, "turn complete") {
		t.Errorf("second call changed result: %s", result)
	}
}

// ===========================================================================
// no_action
// ===========================================================================

func TestNoAction_ReturnsCompletion(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	result := tools.Execute("no_action", `{}`, ctx)
	if !strings.Contains(result, "no action taken") {
		t.Errorf("got: %s", result)
	}
}

func TestNoAction_NoSideEffects(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	tools.Execute("no_action", `{}`, ctx)
	if ctx.DoneCalled {
		t.Error("should not set DoneCalled")
	}
	if ctx.ReplyCalled {
		t.Error("should not set ReplyCalled")
	}
}

// ===========================================================================
// get_current_time
// ===========================================================================

func TestGetCurrentTime_Format(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	result := tools.Execute("get_current_time", `{}`, ctx)

	// Should contain day of week and AM/PM (12-hour format).
	now := time.Now().UTC()
	if !strings.Contains(result, now.Format("Monday")) {
		t.Errorf("expected day of week, got: %s", result)
	}
	if !strings.Contains(result, "AM") && !strings.Contains(result, "PM") {
		t.Errorf("expected 12-hour format with AM/PM, got: %s", result)
	}
}

func TestGetCurrentTime_InvalidTimezone(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	ctx.Cfg.Scheduler.Timezone = "Not/A/Timezone"
	result := tools.Execute("get_current_time", `{}`, ctx)
	// Should fall back to UTC, not error.
	if strings.Contains(result, "error") {
		t.Errorf("expected UTC fallback, got error: %s", result)
	}
	if !strings.Contains(result, "UTC") {
		t.Errorf("expected UTC in fallback result, got: %s", result)
	}
}

// ===========================================================================
// save_fact — happy paths
// ===========================================================================

func TestSaveFact_HappyPath(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	args := `{"fact": "Likes hiking on weekends", "category": "hobbies", "importance": 5, "tags": "outdoors, hiking"}`
	result := tools.Execute("save_fact", args, ctx)

	if !strings.Contains(result, "saved user fact") {
		t.Errorf("expected 'saved user fact', got: %s", result)
	}
	if !strings.Contains(result, "ID=") {
		t.Errorf("expected fact ID in result, got: %s", result)
	}
	if len(ctx.SavedFacts) != 1 {
		t.Errorf("expected 1 saved fact tracked, got %d", len(ctx.SavedFacts))
	}
}

func TestSaveFact_MinimalArgs(t *testing.T) {
	// Only fact is truly required — category, importance, tags are optional.
	ctx := testutil.TestToolContext(t)
	result := tools.Execute("save_fact", `{"fact": "Has a dog"}`, ctx)
	if !strings.Contains(result, "saved user fact") {
		t.Errorf("expected save with minimal args, got: %s", result)
	}
}

// ===========================================================================
// save_fact — error paths
// ===========================================================================

func TestSaveFact_BadJSON(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	// Valid JSON but wrong types — string where int expected.
	result := tools.Execute("save_fact", `{"fact": 12345}`, ctx)
	// json.Unmarshal coerces int→string in Go, so this may or may not error.
	// The important thing is it doesn't panic.
	_ = result
}

func TestSaveFact_EmptyFact(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	result := tools.Execute("save_fact", `{"fact": "", "category": "test", "importance": 5}`, ctx)
	// Empty fact text should either be rejected or saved as empty.
	// It should NOT panic.
	_ = result
}

func TestSaveFact_StyleBlocklist(t *testing.T) {
	cases := []struct {
		name    string
		fact    string
		pattern string // the blocked pattern we expect in the rejection
	}{
		{"em dash", "Autumn loves Go \u2014 especially interfaces", "\u2014"},
		{"en dash", "Go is great \u2013 really great", "\u2013"},
		{"not just", "It's not just a hobby, it's a passion", "not just"},
		{"deeply personal", "Coding is deeply personal to her", "deeply personal"},
		{"delve", "Likes to delve into complex topics", "delve"},
		{"tapestry", "A rich tapestry of interests", "tapestry"},
		{"leverage", "Wants to leverage her skills", "leverage"},
		{"transformative", "A transformative experience", "transformative"},
		{"foster", "Looking to foster better habits", "foster"},
		{"embark", "Embarking on a new journey", "embark"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testutil.TestToolContext(t)
			args, _ := json.Marshal(map[string]any{
				"fact": tc.fact, "category": "test", "importance": 5,
			})
			result := tools.Execute("save_fact", string(args), ctx)
			if !strings.Contains(result, "rejected") {
				t.Errorf("expected style rejection for %q, got: %s", tc.pattern, result)
			}
			if !strings.Contains(result, "plain, concise") {
				t.Errorf("expected rewrite guidance, got: %s", result)
			}
		})
	}
}

func TestSaveFact_TooLong(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	longFact := strings.Repeat("a", 201)
	args := `{"fact": "` + longFact + `", "category": "test", "importance": 3}`
	result := tools.Execute("save_fact", args, ctx)
	if !strings.Contains(result, "rejected") {
		t.Errorf("expected length rejection, got: %s", result)
	}
	if !strings.Contains(result, "201 characters") {
		t.Errorf("expected char count in rejection, got: %s", result)
	}
}

func TestSaveFact_ExactlyAtMaxLength(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	// 200 chars is the limit — should be accepted.
	exactFact := strings.Repeat("b", 200)
	args := `{"fact": "` + exactFact + `", "category": "test", "importance": 3}`
	result := tools.Execute("save_fact", args, ctx)
	if strings.Contains(result, "rejected") {
		t.Errorf("200 chars should be accepted (max is 200), got: %s", result)
	}
}

func TestSaveFact_ImportanceClamping(t *testing.T) {
	t.Run("above max", func(t *testing.T) {
		ctx := testutil.TestToolContext(t)
		result := tools.Execute("save_fact", `{"fact": "Test high", "category": "test", "importance": 99}`, ctx)
		if !strings.Contains(result, "saved") {
			t.Errorf("should clamp to 10, not reject: %s", result)
		}
	})
	t.Run("below min", func(t *testing.T) {
		ctx := testutil.TestToolContext(t)
		result := tools.Execute("save_fact", `{"fact": "Test low", "category": "test", "importance": -5}`, ctx)
		if !strings.Contains(result, "saved") {
			t.Errorf("should clamp to 1, not reject: %s", result)
		}
	})
	t.Run("zero", func(t *testing.T) {
		ctx := testutil.TestToolContext(t)
		result := tools.Execute("save_fact", `{"fact": "Test zero", "category": "test", "importance": 0}`, ctx)
		if !strings.Contains(result, "saved") {
			t.Errorf("should clamp to 1, not reject: %s", result)
		}
	})
}

func TestSaveFact_StripsTimestamps(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		stripped string // substring that should be removed
	}{
		{"today", "Visited the park today", "today"},
		{"yesterday", "Talked to mom yesterday", "yesterday"},
		{"named date", "Went hiking on March 15", "March 15"},
		{"this morning", "Had coffee this morning", "this morning"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testutil.TestToolContext(t)
			args, _ := json.Marshal(map[string]any{
				"fact": tc.input, "category": "context", "importance": 3,
			})
			result := tools.Execute("save_fact", string(args), ctx)
			if !strings.Contains(result, "saved") {
				t.Fatalf("expected save, got: %s", result)
			}
			if strings.Contains(result, tc.stripped) {
				t.Errorf("expected %q stripped from result: %s", tc.stripped, result)
			}
		})
	}
}

func TestSaveFact_DuplicateDetection(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	// Save a fact, then try saving the exact same fact again.
	args := `{"fact": "Has a pet cat named Luna", "category": "pets", "importance": 7, "tags": "pets, cats"}`
	result1 := tools.Execute("save_fact", args, ctx)
	if !strings.Contains(result1, "saved") {
		t.Fatalf("first save failed: %s", result1)
	}
	result2 := tools.Execute("save_fact", args, ctx)
	if !strings.Contains(result2, "rejected") {
		t.Errorf("duplicate should be rejected, got: %s", result2)
	}
	if !strings.Contains(result2, "similar") {
		t.Errorf("rejection should mention similarity, got: %s", result2)
	}
}

func TestSaveFact_ClassifierGate(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	// Wire up a classifier that always rejects.
	ctx.ClassifierLLM = testutil.MockLLMClient(t) // non-nil so gate activates
	ctx.ClassifyWriteFunc = func(writeType, content string, snippet []memory.Message) tools.ClassifyVerdict {
		return tools.ClassifyVerdict{
			Allowed: false,
			Type:    "FICTIONAL",
			Reason:  "this event never happened",
		}
	}
	ctx.RejectionMessageFunc = func(v tools.ClassifyVerdict) string {
		return fmt.Sprintf("rejected by classifier [%s]: %s", v.Type, v.Reason)
	}

	result := tools.Execute("save_fact", `{"fact": "Won the lottery", "category": "events", "importance": 8}`, ctx)
	if !strings.Contains(result, "rejected by classifier") {
		t.Errorf("expected classifier rejection, got: %s", result)
	}
	if !strings.Contains(result, "FICTIONAL") {
		t.Errorf("expected FICTIONAL verdict type, got: %s", result)
	}
}

func TestSaveFact_ClassifierAllows(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	ctx.ClassifierLLM = testutil.MockLLMClient(t)
	ctx.ClassifyWriteFunc = func(writeType, content string, snippet []memory.Message) tools.ClassifyVerdict {
		return tools.ClassifyVerdict{Allowed: true, Type: "SAVE"}
	}

	result := tools.Execute("save_fact", `{"fact": "Has two siblings", "category": "family", "importance": 5}`, ctx)
	if !strings.Contains(result, "saved user fact") {
		t.Errorf("classifier allowed but save failed: %s", result)
	}
}

func TestSaveFact_PreApprovedBypassesClassifier(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	classifierCalled := false
	ctx.ClassifierLLM = testutil.MockLLMClient(t)
	ctx.ClassifyWriteFunc = func(writeType, content string, snippet []memory.Message) tools.ClassifyVerdict {
		classifierCalled = true
		return tools.ClassifyVerdict{Allowed: false, Type: "LOW_VALUE", Reason: "meh"}
	}
	// Pre-approve the exact fact text (lowercase).
	ctx.PreApprovedRewrites["enjoys gardening"] = true

	result := tools.Execute("save_fact", `{"fact": "Enjoys gardening", "category": "hobbies", "importance": 3}`, ctx)
	if classifierCalled {
		t.Error("classifier should have been bypassed for pre-approved rewrite")
	}
	if !strings.Contains(result, "saved") {
		t.Errorf("pre-approved fact should save, got: %s", result)
	}
}

func TestSaveFact_NilEmbedClient(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	ctx.EmbedClient = nil
	// Without embed client, dedup is skipped but save should still work.
	result := tools.Execute("save_fact", `{"fact": "No embed test", "category": "test", "importance": 3}`, ctx)
	if !strings.Contains(result, "saved") {
		t.Errorf("save without embed client should succeed, got: %s", result)
	}
}

// ===========================================================================
// save_self_fact — additional paths
// ===========================================================================

func TestSaveSelfFact_HappyPath(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	result := tools.Execute("save_self_fact", `{"fact": "I notice I use shorter replies when the user seems tired", "category": "communication", "importance": 4}`, ctx)
	if !strings.Contains(result, "saved self fact") {
		t.Errorf("got: %s", result)
	}
}

func TestSaveSelfFact_BlocklistRejects(t *testing.T) {
	cases := []struct {
		name string
		fact string
	}{
		{"can recall", "I can recall past conversations"},
		{"designed to", "I am designed to be helpful"},
		{"my purpose", "My purpose is to assist"},
		{"i can help", "I can help with many tasks"},
		{"i am able to", "I am able to remember things"},
		{"i was created to", "I was created to be a companion"},
		{"i am here to", "I am here to help you"},
		{"i try to be", "I try to be empathetic"},
		{"i should be", "I should be supportive"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testutil.TestToolContext(t)
			args, _ := json.Marshal(map[string]any{
				"fact": tc.fact, "category": "identity", "importance": 5,
			})
			result := tools.Execute("save_self_fact", string(args), ctx)
			if !strings.Contains(result, "rejected") {
				t.Errorf("expected blocklist rejection for %q, got: %s", tc.fact, result)
			}
		})
	}
}

func TestSaveSelfFact_IdentityRestatement(t *testing.T) {
	// Bot name is "TestBot" — both "I am TestBot" and "My name is TestBot"
	// should be caught regardless of case.
	cases := []struct {
		name string
		fact string
	}{
		{"i am name", "I am TestBot"},
		{"my name is", "My name is TestBot"},
		{"lowercase", "i am testbot"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testutil.TestToolContext(t)
			args, _ := json.Marshal(map[string]any{
				"fact": tc.fact, "category": "identity", "importance": 10,
			})
			result := tools.Execute("save_self_fact", string(args), ctx)
			if !strings.Contains(result, "rejected") {
				t.Errorf("expected identity rejection for %q, got: %s", tc.fact, result)
			}
		})
	}
}

func TestSaveSelfFact_StyleGateApplies(t *testing.T) {
	// Style blocklist applies to self-facts too, not just user-facts.
	ctx := testutil.TestToolContext(t)
	result := tools.Execute("save_self_fact", `{"fact": "I find conversations deeply personal", "category": "self", "importance": 4}`, ctx)
	if !strings.Contains(result, "rejected") {
		t.Errorf("style gate should apply to self-facts too, got: %s", result)
	}
}

func TestSaveSelfFact_LengthGateApplies(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	longFact := strings.Repeat("x", 201)
	args, _ := json.Marshal(map[string]any{
		"fact": longFact, "category": "self", "importance": 3,
	})
	result := tools.Execute("save_self_fact", string(args), ctx)
	if !strings.Contains(result, "rejected") {
		t.Errorf("length gate should apply to self-facts, got: %s", result)
	}
}

// ===========================================================================
// recall_memories
// ===========================================================================

func TestRecallMemories_HappyPath(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	tools.Execute("save_fact", `{"fact": "Has a pet cat named Luna", "category": "pets", "importance": 7, "tags": "pets, cats"}`, ctx)

	result := tools.Execute("recall_memories", `{"query": "pets cats"}`, ctx)
	if !strings.Contains(result, "matching memories") {
		t.Errorf("expected matches, got: %s", result)
	}
	if !strings.Contains(result, "Luna") {
		t.Errorf("expected Luna in results, got: %s", result)
	}
}

func TestRecallMemories_NoResults(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	result := tools.Execute("recall_memories", `{"query": "quantum physics"}`, ctx)
	if !strings.Contains(result, "no matching memories") {
		t.Errorf("got: %s", result)
	}
}

func TestRecallMemories_LimitCapped(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	result := tools.Execute("recall_memories", `{"query": "anything", "limit": 100}`, ctx)
	if strings.Contains(result, "error") {
		t.Errorf("high limit should be capped not errored: %s", result)
	}
}

func TestRecallMemories_DefaultLimit(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	// Limit 0 or negative should default to 5, not error.
	result := tools.Execute("recall_memories", `{"query": "anything", "limit": 0}`, ctx)
	if strings.Contains(result, "error") {
		t.Errorf("zero limit should use default: %s", result)
	}
}

func TestRecallMemories_NilEmbedClient(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	ctx.EmbedClient = nil
	result := tools.Execute("recall_memories", `{"query": "anything"}`, ctx)
	if !strings.Contains(result, "not available") {
		t.Errorf("expected 'not available' with nil embed client, got: %s", result)
	}
}

func TestRecallMemories_ZeroEmbedDimension(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	ctx.Store.EmbedDimension = 0
	result := tools.Execute("recall_memories", `{"query": "anything"}`, ctx)
	if !strings.Contains(result, "not available") {
		t.Errorf("expected 'not available' with zero dimension, got: %s", result)
	}
}

func TestRecallMemories_BadJSON(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	result := tools.Execute("recall_memories", `{"query": }`, ctx)
	// Malformed JSON is caught by Execute() before reaching the handler.
	if !strings.Contains(result, "malformed JSON") {
		t.Errorf("expected malformed JSON error, got: %s", result)
	}
}

// ===========================================================================
// remove_fact
// ===========================================================================

func TestRemoveFact_HappyPath(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	saveResult := tools.Execute("save_fact", `{"fact": "Temporary fact", "category": "test", "importance": 1}`, ctx)
	var factID int64
	if _, err := fmt.Sscanf(saveResult, "saved user fact ID=%d", &factID); err != nil {
		t.Fatalf("parse fact ID: %s", saveResult)
	}

	removeArgs, _ := json.Marshal(map[string]any{"fact_id": factID, "reason": "no longer relevant"})
	result := tools.Execute("remove_fact", string(removeArgs), ctx)
	if !strings.Contains(result, "removed fact") {
		t.Errorf("got: %s", result)
	}
}

func TestRemoveFact_WithSupersession(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	// Insert via store directly to bypass dedup (4D embeddings collide).
	id1, err := ctx.Store.SaveFact("Works at Company A", "work", "user", 0, 7, nil, nil, "", "")
	if err != nil {
		t.Fatalf("seed fact 1: %v", err)
	}
	id2, err := ctx.Store.SaveFact("Works at Company B", "work", "user", 0, 7, nil, nil, "", "")
	if err != nil {
		t.Fatalf("seed fact 2: %v", err)
	}

	removeArgs, _ := json.Marshal(map[string]any{
		"fact_id": id1, "reason": "changed jobs", "replaced_by": id2,
	})
	result := tools.Execute("remove_fact", string(removeArgs), ctx)
	if !strings.Contains(result, "superseded") {
		t.Errorf("got: %s", result)
	}
	if !strings.Contains(result, fmt.Sprintf("%d", id1)) {
		t.Errorf("should reference old ID, got: %s", result)
	}
}

func TestRemoveFact_BadJSON(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	result := tools.Execute("remove_fact", `not json`, ctx)
	if !strings.Contains(result, "malformed JSON") {
		t.Errorf("got: %s", result)
	}
}

func TestRemoveFact_SupersedeInvalidTarget(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	id1, _ := ctx.Store.SaveFact("Real fact", "test", "user", 0, 5, nil, nil, "", "")
	// Try to supersede with a nonexistent fact ID — FK should fail.
	removeArgs, _ := json.Marshal(map[string]any{
		"fact_id": id1, "reason": "test", "replaced_by": 99999,
	})
	result := tools.Execute("remove_fact", string(removeArgs), ctx)
	if !strings.Contains(result, "error") {
		t.Errorf("expected FK error for invalid replaced_by, got: %s", result)
	}
}

// ===========================================================================
// update_fact
// ===========================================================================

func TestUpdateFact_HappyPath(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	tools.Execute("save_fact", `{"fact": "Lives in Portland", "category": "location", "importance": 6}`, ctx)

	updateArgs, _ := json.Marshal(map[string]any{
		"fact_id": 1, "fact": "Lives in Seattle", "category": "location", "importance": 6,
	})
	result := tools.Execute("update_fact", string(updateArgs), ctx)
	if !strings.Contains(result, "updated") {
		t.Errorf("got: %s", result)
	}
	if !strings.Contains(result, "superseded") {
		t.Errorf("should create supersession chain, got: %s", result)
	}
}

func TestUpdateFact_NotFound(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	updateArgs, _ := json.Marshal(map[string]any{
		"fact_id": 9999, "fact": "Whatever", "category": "test", "importance": 1,
	})
	result := tools.Execute("update_fact", string(updateArgs), ctx)
	if !strings.Contains(result, "not found") {
		t.Errorf("got: %s", result)
	}
}

func TestUpdateFact_StyleGate(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	ctx.Store.SaveFact("Likes running", "hobbies", "user", 0, 5, nil, nil, "", "")
	updateArgs, _ := json.Marshal(map[string]any{
		"fact_id": 1, "fact": "Likes running \u2014 a transformative experience",
		"category": "hobbies", "importance": 5,
	})
	result := tools.Execute("update_fact", string(updateArgs), ctx)
	if !strings.Contains(result, "rejected") {
		t.Errorf("style gate should fire on update, got: %s", result)
	}
}

func TestUpdateFact_LengthGate(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	ctx.Store.SaveFact("Short fact", "test", "user", 0, 5, nil, nil, "", "")
	longFact := strings.Repeat("z", 201)
	updateArgs, _ := json.Marshal(map[string]any{
		"fact_id": 1, "fact": longFact, "category": "test", "importance": 5,
	})
	result := tools.Execute("update_fact", string(updateArgs), ctx)
	if !strings.Contains(result, "rejected") {
		t.Errorf("length gate should fire on update, got: %s", result)
	}
}

func TestUpdateFact_BadJSON(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	result := tools.Execute("update_fact", `{broken`, ctx)
	if !strings.Contains(result, "malformed JSON") {
		t.Errorf("got: %s", result)
	}
}

func TestUpdateFact_ClassifierRejects(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	ctx.Store.SaveFact("Original fact", "test", "user", 0, 5, nil, nil, "", "")
	ctx.ClassifierLLM = testutil.MockLLMClient(t)
	ctx.ClassifyWriteFunc = func(writeType, content string, snippet []memory.Message) tools.ClassifyVerdict {
		return tools.ClassifyVerdict{Allowed: false, Type: "INFERRED", Reason: "not stated by user"}
	}
	ctx.RejectionMessageFunc = func(v tools.ClassifyVerdict) string {
		return fmt.Sprintf("rejected [%s]: %s", v.Type, v.Reason)
	}

	updateArgs, _ := json.Marshal(map[string]any{
		"fact_id": 1, "fact": "Inferred something", "category": "test", "importance": 5,
	})
	result := tools.Execute("update_fact", string(updateArgs), ctx)
	if !strings.Contains(result, "rejected") {
		t.Errorf("classifier should reject update, got: %s", result)
	}
	if !strings.Contains(result, "INFERRED") {
		t.Errorf("should show verdict type, got: %s", result)
	}
}

func TestUpdateFact_PreApprovedBypassesClassifier(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	ctx.Store.SaveFact("Original", "test", "user", 0, 5, nil, nil, "", "")
	classifierCalled := false
	ctx.ClassifierLLM = testutil.MockLLMClient(t)
	ctx.ClassifyWriteFunc = func(writeType, content string, snippet []memory.Message) tools.ClassifyVerdict {
		classifierCalled = true
		return tools.ClassifyVerdict{Allowed: false}
	}
	ctx.PreApprovedRewrites["updated text here"] = true

	updateArgs, _ := json.Marshal(map[string]any{
		"fact_id": 1, "fact": "Updated text here", "category": "test", "importance": 5,
	})
	result := tools.Execute("update_fact", string(updateArgs), ctx)
	if classifierCalled {
		t.Error("classifier should be bypassed for pre-approved rewrite")
	}
	if !strings.Contains(result, "updated") {
		t.Errorf("pre-approved update should succeed, got: %s", result)
	}
}

// ===========================================================================
// create_schedule
// ===========================================================================

func TestCreateSchedule_HappyPath(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	result := tools.Execute("create_schedule", `{
		"name": "Morning check-in",
		"cron_expr": "0 9 * * *",
		"task_type": "send_message",
		"priority": "normal"
	}`, ctx)

	if !strings.Contains(result, "Schedule #") {
		t.Errorf("got: %s", result)
	}
	if !strings.Contains(result, "Morning check-in") {
		t.Errorf("should echo name, got: %s", result)
	}
	if !strings.Contains(result, "Priority: normal") {
		t.Errorf("should show priority, got: %s", result)
	}
}

func TestCreateSchedule_MedicationAlwaysCritical(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	result := tools.Execute("create_schedule", `{
		"name": "Take meds",
		"cron_expr": "0 8 * * *",
		"task_type": "medication_checkin",
		"priority": "normal"
	}`, ctx)
	if !strings.Contains(result, "Priority: critical") {
		t.Errorf("medication should force critical, got: %s", result)
	}
}

func TestCreateSchedule_DefaultPriority(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	// Omit priority — should default to "normal".
	result := tools.Execute("create_schedule", `{
		"name": "Test",
		"cron_expr": "0 12 * * *",
		"task_type": "send_message"
	}`, ctx)
	if !strings.Contains(result, "Priority: normal") {
		t.Errorf("default priority should be normal, got: %s", result)
	}
}

func TestCreateSchedule_InvalidCron(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	result := tools.Execute("create_schedule", `{
		"name": "Bad", "cron_expr": "not a cron", "task_type": "send_message"
	}`, ctx)
	if !strings.Contains(result, "error") {
		t.Errorf("invalid cron should error, got: %s", result)
	}
}

func TestCreateSchedule_MissingRequiredFields(t *testing.T) {
	cases := []struct {
		name     string
		args     string
		expected string
	}{
		{"missing name", `{"cron_expr": "0 9 * * *", "task_type": "send_message"}`, "name is required"},
		{"missing cron", `{"name": "Test", "task_type": "send_message"}`, "cron_expr is required"},
		{"missing type", `{"name": "Test", "cron_expr": "0 9 * * *"}`, "task_type is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testutil.TestToolContext(t)
			result := tools.Execute("create_schedule", tc.args, ctx)
			if !strings.Contains(result, tc.expected) {
				t.Errorf("expected %q, got: %s", tc.expected, result)
			}
		})
	}
}

func TestCreateSchedule_BadJSON(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	result := tools.Execute("create_schedule", `{bad}`, ctx)
	if !strings.Contains(result, "malformed JSON") {
		t.Errorf("got: %s", result)
	}
}

// ===========================================================================
// update_schedule
// ===========================================================================

func TestUpdateSchedule_Pause(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	tools.Execute("create_schedule", `{
		"name": "To pause", "cron_expr": "0 9 * * *", "task_type": "send_message"
	}`, ctx)

	result := tools.Execute("update_schedule", `{"task_id": 1, "enabled": false}`, ctx)
	if !strings.Contains(result, "paused") {
		t.Errorf("expected 'paused', got: %s", result)
	}
}

func TestUpdateSchedule_Resume(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	tools.Execute("create_schedule", `{
		"name": "To resume", "cron_expr": "0 9 * * *", "task_type": "send_message"
	}`, ctx)
	// Pause then resume.
	tools.Execute("update_schedule", `{"task_id": 1, "enabled": false}`, ctx)
	result := tools.Execute("update_schedule", `{"task_id": 1, "enabled": true}`, ctx)
	if !strings.Contains(result, "resumed") {
		t.Errorf("expected 'resumed', got: %s", result)
	}
}

func TestUpdateSchedule_MissingID(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	result := tools.Execute("update_schedule", `{"enabled": true}`, ctx)
	if !strings.Contains(result, "task_id is required") {
		t.Errorf("got: %s", result)
	}
}

func TestUpdateSchedule_BadJSON(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	result := tools.Execute("update_schedule", `{nope`, ctx)
	if !strings.Contains(result, "malformed JSON") {
		t.Errorf("got: %s", result)
	}
}

// ===========================================================================
// list_schedules
// ===========================================================================

func TestListSchedules_Empty(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	result := tools.Execute("list_schedules", `{}`, ctx)
	if !strings.Contains(result, "No active") {
		t.Errorf("got: %s", result)
	}
}

func TestListSchedules_WithTasks(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	tools.Execute("create_schedule", `{
		"name": "Daily standup", "cron_expr": "30 9 * * 1-5",
		"task_type": "send_message", "priority": "normal"
	}`, ctx)

	result := tools.Execute("list_schedules", `{}`, ctx)
	if !strings.Contains(result, "Daily standup") {
		t.Errorf("should list task name, got: %s", result)
	}
	if !strings.Contains(result, "(1)") {
		t.Errorf("should show count, got: %s", result)
	}
}

func TestListSchedules_PausedTasksNotShown(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	tools.Execute("create_schedule", `{
		"name": "Will pause", "cron_expr": "0 9 * * *", "task_type": "send_message"
	}`, ctx)
	tools.Execute("update_schedule", `{"task_id": 1, "enabled": false}`, ctx)

	result := tools.Execute("list_schedules", `{}`, ctx)
	// ListActiveTasks only returns enabled tasks.
	if !strings.Contains(result, "No active") {
		t.Errorf("paused task should not appear in active list, got: %s", result)
	}
}

// ===========================================================================
// delete_schedule
// ===========================================================================

func TestDeleteSchedule_HappyPath(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	tools.Execute("create_schedule", `{
		"name": "Temporary", "cron_expr": "0 12 * * *", "task_type": "send_message"
	}`, ctx)

	result := tools.Execute("delete_schedule", `{"task_id": 1}`, ctx)
	if !strings.Contains(result, "deleted") {
		t.Errorf("got: %s", result)
	}

	// Verify gone.
	listResult := tools.Execute("list_schedules", `{}`, ctx)
	if !strings.Contains(listResult, "No active") {
		t.Errorf("should be empty after delete, got: %s", listResult)
	}
}

func TestDeleteSchedule_MissingID(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	result := tools.Execute("delete_schedule", `{}`, ctx)
	if !strings.Contains(result, "task_id is required") {
		t.Errorf("got: %s", result)
	}
}

func TestDeleteSchedule_BadJSON(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	result := tools.Execute("delete_schedule", `{invalid`, ctx)
	if !strings.Contains(result, "malformed JSON") {
		t.Errorf("got: %s", result)
	}
}

// ===========================================================================
// create_reminder
// ===========================================================================

func TestCreateReminder_HappyPath(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	seedMessage(t, ctx)

	future := time.Now().Add(1 * time.Hour).UTC().Format("2006-01-02T15:04:05")
	args, _ := json.Marshal(map[string]string{
		"message": "Call the dentist", "trigger_at": future,
	})
	result := tools.Execute("create_reminder", string(args), ctx)
	if !strings.Contains(result, "Reminder #") {
		t.Errorf("got: %s", result)
	}
	if !strings.Contains(result, "Call the dentist") {
		t.Errorf("should echo message, got: %s", result)
	}
}

func TestCreateReminder_PastTime(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	result := tools.Execute("create_reminder", `{"message": "Too late", "trigger_at": "2020-01-01T12:00:00"}`, ctx)
	if !strings.Contains(result, "in the past") {
		t.Errorf("got: %s", result)
	}
}

func TestCreateReminder_MissingMessage(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	result := tools.Execute("create_reminder", `{"trigger_at": "2099-01-01T12:00:00"}`, ctx)
	if !strings.Contains(result, "message is required") {
		t.Errorf("got: %s", result)
	}
}

func TestCreateReminder_MissingTriggerAt(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	result := tools.Execute("create_reminder", `{"message": "Do something"}`, ctx)
	if !strings.Contains(result, "trigger_at is required") {
		t.Errorf("got: %s", result)
	}
}

func TestCreateReminder_BadTimestamp(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	result := tools.Execute("create_reminder", `{"message": "Test", "trigger_at": "not-a-date"}`, ctx)
	if !strings.Contains(result, "error") {
		t.Errorf("expected parse error for bad timestamp, got: %s", result)
	}
}

func TestCreateReminder_RFC3339Format(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	seedMessage(t, ctx)

	// RFC3339 with timezone offset should be accepted.
	future := time.Now().Add(2 * time.Hour).Format(time.RFC3339)
	args, _ := json.Marshal(map[string]string{
		"message": "RFC3339 test", "trigger_at": future,
	})
	result := tools.Execute("create_reminder", string(args), ctx)
	if !strings.Contains(result, "Reminder #") {
		t.Errorf("RFC3339 format should be accepted, got: %s", result)
	}
}

func TestCreateReminder_BadJSON(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	result := tools.Execute("create_reminder", `{broken`, ctx)
	if !strings.Contains(result, "malformed JSON") {
		t.Errorf("got: %s", result)
	}
}

// ===========================================================================
// view_image — nil guards
// ===========================================================================

func TestViewImage_NoImage(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	// No image attached — should return clear error, not panic.
	result := tools.Execute("view_image", `{"prompt": "describe this"}`, ctx)
	if !strings.Contains(result, "No image") {
		t.Errorf("expected 'No image' error, got: %s", result)
	}
}

func TestViewImage_NoVisionModel(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	ctx.ImageBase64 = "base64data"
	ctx.ImageMIME = "image/jpeg"
	// VisionLLM is nil by default.
	result := tools.Execute("view_image", `{"prompt": "describe this"}`, ctx)
	if !strings.Contains(result, "not configured") {
		t.Errorf("expected 'not configured' for nil vision LLM, got: %s", result)
	}
}

// ===========================================================================
// search_history — nil guards and validation
// ===========================================================================

func TestSearchHistory_NoSkillRegistry(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	result := tools.Execute("search_history", `{"skill_name": "web_search", "query": "test"}`, ctx)
	if !strings.Contains(result, "not initialized") {
		t.Errorf("got: %s", result)
	}
}

func TestSearchHistory_NilEmbedClient(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	ctx.EmbedClient = nil
	// SkillRegistry is already nil, so we'd hit that guard first.
	// This just verifies we don't panic when both are nil.
	result := tools.Execute("search_history", `{"skill_name": "test", "query": "test"}`, ctx)
	if strings.Contains(result, "panic") {
		t.Errorf("should not panic: %s", result)
	}
}

func TestSearchHistory_MissingArgs(t *testing.T) {
	ctx := testutil.TestToolContext(t)
	result := tools.Execute("search_history", `{}`, ctx)
	// Should hit nil registry guard before arg validation.
	if strings.Contains(result, "panic") {
		t.Errorf("should not panic on empty args: %s", result)
	}
}

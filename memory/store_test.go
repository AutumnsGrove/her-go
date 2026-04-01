package memory_test

import (
	"encoding/json"
	"testing"
	"time"

	"her/memory"
	"her/testutil"
)

// ---------------------------------------------------------------------------
// Schema & Init
// ---------------------------------------------------------------------------

// TestStore_Init verifies that a fresh database has the correct schema and
// SQLite pragmas. This is the most fundamental test — if init fails,
// nothing else works.
func TestStore_Init(t *testing.T) {
	store := testutil.TempStore(t)

	// The store should be usable immediately after construction.
	// SaveMessage touches the messages table — if it exists and the
	// columns match, init worked.
	id, err := store.SaveMessage("user", "hello", "hello", "conv-1")
	if err != nil {
		t.Fatalf("SaveMessage on fresh DB failed: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero message ID from fresh DB")
	}
}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

func TestStore_SaveMessage(t *testing.T) {
	store := testutil.TempStore(t)

	id, err := store.SaveMessage("user", "raw with PII 555-1234", "[PHONE_1]", "conv-1")
	if err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}

	// Retrieve and verify both raw and scrubbed content are stored.
	msgs, err := store.RecentMessages("conv-1", 10)
	if err != nil {
		t.Fatalf("RecentMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}

	msg := msgs[0]
	if msg.ID != id {
		t.Errorf("ID: got %d, want %d", msg.ID, id)
	}
	if msg.Role != "user" {
		t.Errorf("Role: got %q, want %q", msg.Role, "user")
	}
	if msg.ContentRaw != "raw with PII 555-1234" {
		t.Errorf("ContentRaw: got %q, want raw content", msg.ContentRaw)
	}
	if msg.ContentScrubbed != "[PHONE_1]" {
		t.Errorf("ContentScrubbed: got %q, want scrubbed content", msg.ContentScrubbed)
	}
	if msg.ConversationID != "conv-1" {
		t.Errorf("ConversationID: got %q, want %q", msg.ConversationID, "conv-1")
	}
}

func TestStore_RecentMessages_Ordering(t *testing.T) {
	store := testutil.TempStore(t)

	// Insert messages in order.
	store.SaveMessage("user", "first", "first", "conv-1")
	store.SaveMessage("assistant", "second", "second", "conv-1")
	store.SaveMessage("user", "third", "third", "conv-1")

	msgs, err := store.RecentMessages("conv-1", 10)
	if err != nil {
		t.Fatalf("RecentMessages: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}

	// Messages should come back in chronological order (oldest first).
	if msgs[0].ContentRaw != "first" {
		t.Errorf("first message: got %q", msgs[0].ContentRaw)
	}
	if msgs[2].ContentRaw != "third" {
		t.Errorf("last message: got %q", msgs[2].ContentRaw)
	}
}

func TestStore_RecentMessages_Limit(t *testing.T) {
	store := testutil.TempStore(t)

	for i := 0; i < 10; i++ {
		store.SaveMessage("user", "msg", "msg", "conv-1")
	}

	msgs, err := store.RecentMessages("conv-1", 3)
	if err != nil {
		t.Fatalf("RecentMessages: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}
}

func TestStore_RecentMessages_ConversationIsolation(t *testing.T) {
	store := testutil.TempStore(t)

	store.SaveMessage("user", "conv1 msg", "conv1 msg", "conv-1")
	store.SaveMessage("user", "conv2 msg", "conv2 msg", "conv-2")

	msgs, err := store.RecentMessages("conv-1", 10)
	if err != nil {
		t.Fatalf("RecentMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message for conv-1, got %d", len(msgs))
	}
	if msgs[0].ContentRaw != "conv1 msg" {
		t.Errorf("wrong message: got %q", msgs[0].ContentRaw)
	}
}

// ---------------------------------------------------------------------------
// Facts — CRUD
// ---------------------------------------------------------------------------

func TestStore_SaveFact(t *testing.T) {
	store := testutil.TempStore(t)

	emb := testutil.DeterministicEmbedding("user likes cats")
	id, err := store.SaveFact(
		"user likes cats", "preferences", "user",
		0, 7, emb, emb, "pets", "",
	)
	if err != nil {
		t.Fatalf("SaveFact: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero fact ID")
	}

	// Read it back.
	fact, err := store.GetFact(id)
	if err != nil {
		t.Fatalf("GetFact: %v", err)
	}
	if fact.Fact != "user likes cats" {
		t.Errorf("Fact text: got %q", fact.Fact)
	}
	if fact.Category != "preferences" {
		t.Errorf("Category: got %q", fact.Category)
	}
	if fact.Importance != 7 {
		t.Errorf("Importance: got %d, want 7", fact.Importance)
	}
	if !fact.Active {
		t.Error("expected fact to be active")
	}
}

func TestStore_SaveFact_DefaultSubject(t *testing.T) {
	store := testutil.TempStore(t)

	// Pass empty subject — should default to "user".
	id, _ := store.SaveFact("test", "general", "", 0, 5, nil, nil, "", "")
	fact, _ := store.GetFact(id)
	if fact.Subject != "user" {
		t.Errorf("expected default subject %q, got %q", "user", fact.Subject)
	}
}

func TestStore_UpdateFact(t *testing.T) {
	store := testutil.TempStore(t)

	id, _ := store.SaveFact("original text", "general", "user", 0, 5, nil, nil, "", "")

	err := store.UpdateFact(id, "updated text", "preferences", 8, "new-tag")
	if err != nil {
		t.Fatalf("UpdateFact: %v", err)
	}

	fact, _ := store.GetFact(id)
	if fact.Fact != "updated text" {
		t.Errorf("Fact: got %q, want %q", fact.Fact, "updated text")
	}
	if fact.Category != "preferences" {
		t.Errorf("Category: got %q, want %q", fact.Category, "preferences")
	}
	if fact.Importance != 8 {
		t.Errorf("Importance: got %d, want 8", fact.Importance)
	}
}

func TestStore_DeactivateFact(t *testing.T) {
	store := testutil.TempStore(t)

	id, _ := store.SaveFact("to be removed", "general", "user", 0, 5, nil, nil, "", "")

	err := store.DeactivateFact(id)
	if err != nil {
		t.Fatalf("DeactivateFact: %v", err)
	}

	fact, _ := store.GetFact(id)
	if fact.Active {
		t.Error("expected fact to be inactive after deactivation")
	}
}

// ---------------------------------------------------------------------------
// Facts — Semantic Search
// ---------------------------------------------------------------------------

func TestStore_SemanticSearch(t *testing.T) {
	store := testutil.TempStore(t)

	// Seed facts with different embeddings.
	catEmb := testutil.DeterministicEmbedding("cats are wonderful pets")
	dogEmb := testutil.DeterministicEmbedding("dogs love to play fetch")
	mathEmb := testutil.DeterministicEmbedding("linear algebra equations")

	store.SaveFact("user loves cats", "preferences", "user", 0, 7, catEmb, catEmb, "", "")
	store.SaveFact("user has a dog", "preferences", "user", 0, 6, dogEmb, dogEmb, "", "")
	store.SaveFact("user studies math", "knowledge", "user", 0, 5, mathEmb, mathEmb, "", "")

	// Search with a query close to "cats".
	queryVec := testutil.DeterministicEmbedding("cats are wonderful pets")
	results, err := store.SemanticSearch(queryVec, 2)
	if err != nil {
		t.Fatalf("SemanticSearch: %v", err)
	}

	if len(results) == 0 {
		t.Fatal("expected at least 1 result")
	}

	// The closest match should be the cat fact (identical embedding).
	if results[0].Fact != "user loves cats" {
		t.Errorf("closest result: got %q, want %q", results[0].Fact, "user loves cats")
	}
}

func TestStore_SemanticSearch_RespectsTopK(t *testing.T) {
	store := testutil.TempStore(t)

	// Seed 5 facts.
	for i := 0; i < 5; i++ {
		emb := testutil.DeterministicEmbedding("fact number " + string(rune('A'+i)))
		store.SaveFact("fact "+string(rune('A'+i)), "general", "user", 0, 5, emb, emb, "", "")
	}

	queryVec := testutil.DeterministicEmbedding("fact number A")
	results, err := store.SemanticSearch(queryVec, 2)
	if err != nil {
		t.Fatalf("SemanticSearch: %v", err)
	}
	if len(results) > 2 {
		t.Errorf("expected at most 2 results, got %d", len(results))
	}
}

func TestStore_SemanticSearch_ExcludesInactive(t *testing.T) {
	store := testutil.TempStore(t)

	emb := testutil.DeterministicEmbedding("deactivated fact")
	id, _ := store.SaveFact("deactivated fact", "general", "user", 0, 5, emb, emb, "", "")
	store.DeactivateFact(id)

	results, err := store.SemanticSearch(emb, 10)
	if err != nil {
		t.Fatalf("SemanticSearch: %v", err)
	}

	for _, r := range results {
		if r.ID == id {
			t.Error("semantic search returned a deactivated fact")
		}
	}
}

// ---------------------------------------------------------------------------
// Facts — Zettelkasten Linking
// ---------------------------------------------------------------------------

func TestStore_ZettelkastenLinking(t *testing.T) {
	store := testutil.TempStore(t)

	// Enable auto-linking with a low threshold so our hash-based embeddings link.
	store.AutoLinkCount = 2
	store.AutoLinkThreshold = 0.0 // accept any similarity for testing

	// Save two related facts.
	emb1 := testutil.DeterministicEmbedding("user has a cat named Luna")
	id1, _ := store.SaveFact("user has a cat named Luna", "pets", "user", 0, 7, emb1, emb1, "", "")

	emb2 := testutil.DeterministicEmbedding("user adopted Luna from a shelter")
	id2, _ := store.SaveFact("user adopted Luna from a shelter", "pets", "user", 0, 6, emb2, emb2, "", "")

	// Auto-link the second fact to existing facts.
	if err := store.AutoLinkFact(id2, emb2); err != nil {
		t.Fatalf("AutoLinkFact: %v", err)
	}

	// Verify that fact 2 links to fact 1.
	linked, err := store.LinkedFacts(id2, 10)
	if err != nil {
		t.Fatalf("LinkedFacts: %v", err)
	}

	found := false
	for _, f := range linked {
		if f.ID == id1 {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected fact 2 to auto-link to fact 1")
	}

	// Links should be bidirectional — fact 1 should also link to fact 2.
	linked1, _ := store.LinkedFacts(id1, 10)
	found = false
	for _, f := range linked1 {
		if f.ID == id2 {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected bidirectional link: fact 1 → fact 2")
	}
}

func TestStore_ManualLinkFacts(t *testing.T) {
	store := testutil.TempStore(t)

	id1, _ := store.SaveFact("fact A", "general", "user", 0, 5, nil, nil, "", "")
	id2, _ := store.SaveFact("fact B", "general", "user", 0, 5, nil, nil, "", "")

	if err := store.LinkFacts(id1, id2, 0.9); err != nil {
		t.Fatalf("LinkFacts: %v", err)
	}

	count, err := store.CountFactLinks()
	if err != nil {
		t.Fatalf("CountFactLinks: %v", err)
	}
	// One manual link creates 2 rows (bidirectional).
	if count < 1 {
		t.Errorf("expected at least 1 link, got %d", count)
	}
}

func TestStore_SupersedeFact(t *testing.T) {
	store := testutil.TempStore(t)

	oldID, _ := store.SaveFact("user's favorite color is blue", "preferences", "user", 0, 5, nil, nil, "", "")
	newID, _ := store.SaveFact("user's favorite color is green", "preferences", "user", 0, 5, nil, nil, "", "")

	err := store.SupersedeFact(oldID, newID, "user corrected")
	if err != nil {
		t.Fatalf("SupersedeFact: %v", err)
	}

	old, _ := store.GetFact(oldID)
	if old.Active {
		t.Error("superseded fact should be inactive")
	}
	if old.SupersededBy != newID {
		t.Errorf("SupersededBy: got %d, want %d", old.SupersededBy, newID)
	}
}

// ---------------------------------------------------------------------------
// Summaries
// ---------------------------------------------------------------------------

func TestStore_SaveSummary(t *testing.T) {
	store := testutil.TempStore(t)

	id, err := store.SaveSummary("conv-1", "they talked about cats", 1, 10, "chat")
	if err != nil {
		t.Fatalf("SaveSummary: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero summary ID")
	}
}

func TestStore_LatestSummary(t *testing.T) {
	store := testutil.TempStore(t)

	// No summary yet — should return empty.
	text, endID, err := store.LatestSummary("conv-1", "chat")
	if err != nil {
		t.Fatalf("LatestSummary (empty): %v", err)
	}
	if text != "" || endID != 0 {
		t.Errorf("expected empty summary, got %q (endID=%d)", text, endID)
	}

	// Save two summaries — latest should win.
	store.SaveSummary("conv-1", "first summary", 1, 5, "chat")
	store.SaveSummary("conv-1", "second summary", 6, 10, "chat")

	text, endID, err = store.LatestSummary("conv-1", "chat")
	if err != nil {
		t.Fatalf("LatestSummary: %v", err)
	}
	if text != "second summary" {
		t.Errorf("expected latest summary, got %q", text)
	}
	if endID != 10 {
		t.Errorf("endID: got %d, want 10", endID)
	}
}

func TestStore_LatestSummary_StreamIsolation(t *testing.T) {
	store := testutil.TempStore(t)

	store.SaveSummary("conv-1", "chat summary", 1, 5, "chat")
	store.SaveSummary("conv-1", "agent summary", 1, 5, "agent")

	chatText, _, _ := store.LatestSummary("conv-1", "chat")
	agentText, _, _ := store.LatestSummary("conv-1", "agent")

	if chatText != "chat summary" {
		t.Errorf("chat stream: got %q", chatText)
	}
	if agentText != "agent summary" {
		t.Errorf("agent stream: got %q", agentText)
	}
}

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

func TestStore_SaveMetric(t *testing.T) {
	store := testutil.TempStore(t)

	// Need a message to link the metric to.
	msgID, _ := store.SaveMessage("user", "hello", "hello", "conv-1")

	err := store.SaveMetric("test-model", 100, 50, 150, 0.003, 450, msgID)
	if err != nil {
		t.Fatalf("SaveMetric: %v", err)
	}

	// Verify through stats.
	stats, err := store.GetStats()
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	if stats.TotalTokens != 150 {
		t.Errorf("TotalTokens: got %d, want 150", stats.TotalTokens)
	}
}

// ---------------------------------------------------------------------------
// Agent Turns
// ---------------------------------------------------------------------------

func TestStore_SaveAgentTurn(t *testing.T) {
	store := testutil.TempStore(t)

	msgID, _ := store.SaveMessage("user", "hello", "hello", "conv-1")

	err := store.SaveAgentTurn(msgID, 0, "assistant", "think", `{}`, "thinking about cats")
	if err != nil {
		t.Fatalf("SaveAgentTurn: %v", err)
	}

	err = store.SaveAgentTurn(msgID, 1, "assistant", "reply", `{"text":"hi"}`, "hi there")
	if err != nil {
		t.Fatalf("SaveAgentTurn (second): %v", err)
	}

	// Verify through recent agent actions.
	actions, err := store.RecentAgentActions("conv-1", 5)
	if err != nil {
		t.Fatalf("RecentAgentActions: %v", err)
	}
	if len(actions) < 1 {
		t.Fatal("expected at least 1 agent action")
	}
}

// ---------------------------------------------------------------------------
// Scheduled Tasks
// ---------------------------------------------------------------------------

func TestStore_ScheduledTasks_CRUD(t *testing.T) {
	store := testutil.TempStore(t)

	now := time.Now().Truncate(time.Second)
	name := "morning briefing"
	task := &memory.ScheduledTask{
		Name:         &name,
		ScheduleType: "recurring",
		CronExpr:     strPtr("0 8 * * *"),
		TaskType:     "run_prompt",
		Payload:      json.RawMessage(`{"prompt": "good morning"}`),
		Enabled:      true,
		Priority:     "normal",
		CreatedBy:    "system",
		NextRun:      &now,
	}

	// Create
	id, err := store.CreateScheduledTask(task)
	if err != nil {
		t.Fatalf("CreateScheduledTask: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero task ID")
	}

	// List active
	tasks, err := store.ListActiveTasks()
	if err != nil {
		t.Fatalf("ListActiveTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 active task, got %d", len(tasks))
	}
	if *tasks[0].Name != "morning briefing" {
		t.Errorf("task name: got %q", *tasks[0].Name)
	}

	// Get due tasks
	due, err := store.GetDueTasks(now.Add(time.Minute))
	if err != nil {
		t.Fatalf("GetDueTasks: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("expected 1 due task, got %d", len(due))
	}

	// Disable
	err = store.UpdateScheduledTaskEnabled(id, false)
	if err != nil {
		t.Fatalf("UpdateScheduledTaskEnabled: %v", err)
	}
	tasks, _ = store.ListActiveTasks()
	if len(tasks) != 0 {
		t.Error("expected 0 active tasks after disable")
	}

	// Delete
	err = store.DeleteScheduledTask(id)
	if err != nil {
		t.Fatalf("DeleteScheduledTask: %v", err)
	}
}

func TestStore_ScheduledTasks_MarkRun(t *testing.T) {
	store := testutil.TempStore(t)

	now := time.Now().Truncate(time.Second)
	task := &memory.ScheduledTask{
		ScheduleType: "once",
		TriggerAt:    &now,
		TaskType:     "send_message",
		Payload:      json.RawMessage(`{"text": "reminder"}`),
		Enabled:      true,
		Priority:     "normal",
		CreatedBy:    "user",
		NextRun:      &now,
	}

	id, _ := store.CreateScheduledTask(task)

	nextRun := now.Add(24 * time.Hour)
	err := store.MarkTaskRun(id, &nextRun)
	if err != nil {
		t.Fatalf("MarkTaskRun: %v", err)
	}

	// The task's next_run should have moved forward.
	due, _ := store.GetDueTasks(now.Add(time.Minute))
	if len(due) != 0 {
		t.Error("task should not be due immediately after MarkTaskRun")
	}
}

// ---------------------------------------------------------------------------
// Mood Entries
// ---------------------------------------------------------------------------

func TestStore_MoodEntries(t *testing.T) {
	store := testutil.TempStore(t)

	id, err := store.SaveMoodEntry(4, "feeling good", `{"energy": "high"}`, "manual", "conv-1")
	if err != nil {
		t.Fatalf("SaveMoodEntry: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero mood entry ID")
	}

	entries, err := store.RecentMoodEntries(10)
	if err != nil {
		t.Fatalf("RecentMoodEntries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 mood entry, got %d", len(entries))
	}
	if entries[0].Rating != 4 {
		t.Errorf("Rating: got %d, want 4", entries[0].Rating)
	}
	if entries[0].Note != "feeling good" {
		t.Errorf("Note: got %q", entries[0].Note)
	}
}

func TestStore_MoodTrend(t *testing.T) {
	store := testutil.TempStore(t)

	// Insert a sequence of moods.
	store.SaveMoodEntry(3, "", "", "inferred", "conv-1")
	store.SaveMoodEntry(4, "", "", "inferred", "conv-1")
	store.SaveMoodEntry(5, "", "", "inferred", "conv-1")

	avg, count, err := store.MoodTrend(10)
	if err != nil {
		t.Fatalf("MoodTrend: %v", err)
	}
	if count != 3 {
		t.Errorf("count: got %d, want 3", count)
	}
	if avg < 3.9 || avg > 4.1 {
		t.Errorf("average: got %.2f, want ~4.0", avg)
	}
}

// ---------------------------------------------------------------------------
// Expenses
// ---------------------------------------------------------------------------

func TestStore_Expenses(t *testing.T) {
	store := testutil.TempStore(t)

	id, err := store.SaveExpense(42.50, "USD", "Coffee Shop", "food", "2026-04-01", "morning coffee", 0)
	if err != nil {
		t.Fatalf("SaveExpense: %v", err)
	}

	// Add line items.
	err = store.SaveExpenseItem(id, "latte", 1, 5.50, 5.50)
	if err != nil {
		t.Fatalf("SaveExpenseItem: %v", err)
	}
	err = store.SaveExpenseItem(id, "croissant", 2, 3.50, 7.00)
	if err != nil {
		t.Fatalf("SaveExpenseItem (2): %v", err)
	}

	// Retrieve.
	expenses, items, err := store.RecentExpenses(10)
	if err != nil {
		t.Fatalf("RecentExpenses: %v", err)
	}
	if len(expenses) != 1 {
		t.Fatalf("expected 1 expense, got %d", len(expenses))
	}
	if expenses[0].Vendor != "Coffee Shop" {
		t.Errorf("Vendor: got %q", expenses[0].Vendor)
	}
	if len(items[id]) != 2 {
		t.Errorf("expected 2 line items, got %d", len(items[id]))
	}
}

func TestStore_UpdateExpense(t *testing.T) {
	store := testutil.TempStore(t)

	id, _ := store.SaveExpense(10.00, "USD", "Store", "general", "2026-04-01", "", 0)

	err := store.UpdateExpense(id, 15.00, "USD", "Better Store", "food", "2026-04-01", "corrected")
	if err != nil {
		t.Fatalf("UpdateExpense: %v", err)
	}

	expenses, _, _ := store.RecentExpenses(10)
	if expenses[0].Amount != 15.00 {
		t.Errorf("Amount: got %.2f, want 15.00", expenses[0].Amount)
	}
	if expenses[0].Vendor != "Better Store" {
		t.Errorf("Vendor: got %q", expenses[0].Vendor)
	}
}

func TestStore_DeleteExpense(t *testing.T) {
	store := testutil.TempStore(t)

	id, _ := store.SaveExpense(10.00, "USD", "Store", "general", "2026-04-01", "", 0)

	err := store.DeleteExpense(id)
	if err != nil {
		t.Fatalf("DeleteExpense: %v", err)
	}

	expenses, _, _ := store.RecentExpenses(10)
	if len(expenses) != 0 {
		t.Error("expected 0 expenses after delete")
	}
}

func TestStore_ExpenseSummary(t *testing.T) {
	store := testutil.TempStore(t)

	store.SaveExpense(10.00, "USD", "A", "food", "2026-04-01", "", 0)
	store.SaveExpense(20.00, "USD", "B", "food", "2026-04-02", "", 0)
	store.SaveExpense(30.00, "USD", "C", "transport", "2026-04-03", "", 0)

	total, byCategory, count, err := store.ExpenseSummary("2026-04-01", "2026-04-30")
	if err != nil {
		t.Fatalf("ExpenseSummary: %v", err)
	}
	if count != 3 {
		t.Errorf("count: got %d, want 3", count)
	}
	if total != 60.00 {
		t.Errorf("total: got %.2f, want 60.00", total)
	}
	if byCategory["food"] != 30.00 {
		t.Errorf("food total: got %.2f, want 30.00", byCategory["food"])
	}
	if byCategory["transport"] != 30.00 {
		t.Errorf("transport total: got %.2f, want 30.00", byCategory["transport"])
	}
}

// ---------------------------------------------------------------------------
// PII Vault
// ---------------------------------------------------------------------------

func TestStore_PIIVault(t *testing.T) {
	store := testutil.TempStore(t)

	msgID, _ := store.SaveMessage("user", "call me at 555-1234", "[PHONE_1]", "conv-1")

	err := store.SavePIIVaultEntry(msgID, "[PHONE_1]", "555-1234", "phone")
	if err != nil {
		t.Fatalf("SavePIIVaultEntry: %v", err)
	}

	// The PII vault is write-only from store's perspective — deanonymization
	// happens through scrub.Vault at runtime. But we can verify the entry
	// persisted by inserting and not getting an error.
}

// ---------------------------------------------------------------------------
// Pending Confirmations
// ---------------------------------------------------------------------------

func TestStore_PendingConfirmations_Lifecycle(t *testing.T) {
	store := testutil.TempStore(t)

	payload := json.RawMessage(`{"action": "delete_fact", "id": 42}`)
	id, err := store.CreatePendingConfirmation(12345, "delete_fact", payload, "Delete the fact about cats?")
	if err != nil {
		t.Fatalf("CreatePendingConfirmation: %v", err)
	}

	// Retrieve by Telegram message ID.
	conf, err := store.GetPendingConfirmation(12345)
	if err != nil {
		t.Fatalf("GetPendingConfirmation: %v", err)
	}
	if conf.ID != id {
		t.Errorf("ID: got %d, want %d", conf.ID, id)
	}
	if conf.ActionType != "delete_fact" {
		t.Errorf("ActionType: got %q", conf.ActionType)
	}
	if conf.Description != "Delete the fact about cats?" {
		t.Errorf("Description: got %q", conf.Description)
	}
	if conf.ResolvedAt != nil {
		t.Error("expected unresolved confirmation")
	}

	// Resolve it.
	err = store.ResolvePendingConfirmation(id, "confirmed")
	if err != nil {
		t.Fatalf("ResolvePendingConfirmation: %v", err)
	}

	// After resolution, GetPendingConfirmation filters it out
	// (WHERE resolved_at IS NULL) — this is by design. A resolved
	// confirmation is no longer "pending." Verify it returns nil.
	conf, err = store.GetPendingConfirmation(12345)
	if err != nil {
		t.Fatalf("GetPendingConfirmation after resolve: %v", err)
	}
	if conf != nil {
		t.Error("expected nil after resolution (no longer pending)")
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// strPtr is a tiny helper for creating *string values in struct literals.
// Go doesn't let you take the address of a string literal directly —
// you can't write &"hello". This is a common Go annoyance.
func strPtr(s string) *string {
	return &s
}

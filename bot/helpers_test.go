package bot

// TestGetConversationID_Concurrent verifies that getConversationID is
// safe under concurrent access. Ten goroutines all call it for the
// same chatID at the same time — without the double-checked locking fix
// they could each create a different conversation ID. With it, they
// must all converge on the same ID.
//
// Run with: go test -race -run TestGetConversationID_Concurrent ./bot/

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"her/memory"
)

// stubStore is a minimal memory.Store that always reports no existing
// conversation ID. All other methods are no-ops or zero-value returns
// so the compiler is satisfied — we only need LatestConversationID
// for this test.
//
// In Go, interface satisfaction is implicit: any type that provides
// all the methods the interface declares automatically satisfies it.
// No "implements" keyword needed. This lets us build a test double
// with just the behaviour we care about.
type stubStore struct{}

func (s stubStore) Close() error                                   { return nil }
func (s stubStore) SaveMessage(role, contentRaw, contentScrubbed, conversationID string) (int64, error) {
	return 0, nil
}
func (s stubStore) GlobalRecentMessages(limit int) ([]memory.Message, error) { return nil, nil }
func (s stubStore) RecentMessages(conversationID string, limit int) ([]memory.Message, error) {
	return nil, nil
}
func (s stubStore) MessagesAfter(conversationID string, sinceID int64) ([]memory.Message, error) {
	return nil, nil
}
func (s stubStore) MessagesInRange(conversationID string, startID, endID int64) ([]memory.Message, error) {
	return nil, nil
}
func (s stubStore) UpdateMessageScrubbed(messageID int64, scrubbed string) error        { return nil }
func (s stubStore) UpdateMessageMedia(messageID int64, fileID, description string) error { return nil }
func (s stubStore) UpdateMessageVoicePath(messageID int64, path string) error            { return nil }
func (s stubStore) UpdateMessageTokenCount(messageID int64, tokenCount int) error        { return nil }
func (s stubStore) MessageCountSince(conversationID string, sinceID int64) (int, error) {
	return 0, nil
}
func (s stubStore) ConversationCountSince(since time.Time) (int, error) { return 0, nil }

// LatestConversationID returns "" — simulating a fresh start with no
// existing conversation in the database. This forces getConversationID
// to take the "create new ID" branch, which is the race-prone path.
func (s stubStore) LatestConversationID(prefix string) string { return "" }

func (s stubStore) LastExtractionMessageID() (int64, error) { return 0, nil }
func (s stubStore) SaveMemory(content, category, subject string, sourceMessageID int64, importance int, embedding []float32, embeddingText []float32, tags string, context string, cardID int64) (int64, error) {
	return 0, nil
}
func (s stubStore) UpdateMemoryEmbedding(memoryID int64, embedding []float32, embeddingText []float32) error {
	return nil
}
func (s stubStore) RecentMemories(subject string, limit int) ([]memory.Memory, error) { return nil, nil }
func (s stubStore) GetMemoryContent(memoryID int64) (string, error)                   { return "", nil }
func (s stubStore) UpdateMemory(memoryID int64, content, category string, importance int, tags string) error {
	return nil
}
func (s stubStore) UpdateMemoryTags(memoryID int64, tags string) error { return nil }
func (s stubStore) DeactivateMemory(memoryID int64) error               { return nil }
func (s stubStore) LinkMemories(id1, id2 int64, similarity float64) error { return nil }
func (s stubStore) LinkedMemories(memoryID int64, limit int) ([]memory.Memory, error) {
	return nil, nil
}
func (s stubStore) AutoLinkMemory(memoryID int64, embedding []float32) error { return nil }
func (s stubStore) SupersedeMemory(oldID, newID int64, reason string) error  { return nil }
func (s stubStore) GetMemory(memoryID int64) (*memory.Memory, error)         { return nil, nil }
func (s stubStore) MemoryHistory(memoryID int64) ([]memory.Memory, error)    { return nil, nil }
func (s stubStore) CountMemoryLinks() (int, error)                           { return 0, nil }
func (s stubStore) AllActiveMemories() ([]memory.Memory, error)              { return nil, nil }
func (s stubStore) SemanticSearch(queryVec []float32, topK int) ([]memory.Memory, error) {
	return nil, nil
}
func (s stubStore) MemoriesWithoutEmbeddings() ([]memory.Memory, error) { return nil, nil }
func (s stubStore) VecMemoriesCount() (int, error)                      { return 0, nil }
func (s stubStore) FindMemoriesByKeyword(keyword string) ([]memory.Memory, error) {
	return nil, nil
}
func (s stubStore) SaveDreamAudit(op string, sourceIDs []int64, resultID int64, before, after, reason string, dryRun bool) error {
	return nil
}
func (s stubStore) RecentDreamAudits(limit int) ([]memory.DreamAudit, error) { return nil, nil }
func (s stubStore) RecentAgentActions(conversationID string, messageLimit int) ([]memory.AgentAction, error) {
	return nil, nil
}
func (s stubStore) SaveAgentTurn(messageID int64, turnIndex int, role, toolName, toolArgs, content string) error {
	return nil
}
func (s stubStore) SaveSearch(messageID int64, searchType, query, results string, resultCount int) error {
	return nil
}
func (s stubStore) SaveClassifierLog(conversationID, writeType, verdict, content, reason, rewrite string) error {
	return nil
}
func (s stubStore) LogCommand(command string, chatID int64, conversationID, args string) {}
func (s stubStore) PersonaHistory(limit int) ([]memory.PersonaVersion, error)           { return nil, nil }
func (s stubStore) SavePersonaVersion(content, trigger string) (int64, error)           { return 0, nil }
func (s stubStore) SaveReflection(content string, factCount int, userMessage, miraResponse string) (int64, error) {
	return 0, nil
}
func (s stubStore) FactCountSinceLastReflection() (int, error) { return 0, nil }
func (s stubStore) TotalReflectionCount() (int, error)         { return 0, nil }
func (s stubStore) PersonaRewriteCount() (int, error)          { return 0, nil }
func (s stubStore) LastPersonaTimestamp() (time.Time, error)   { return time.Time{}, nil }
func (s stubStore) ReflectionsSince(since time.Time) ([]memory.Reflection, error) {
	return nil, nil
}
func (s stubStore) SaveTraits(traits []memory.Trait, personaVersionID int64) error { return nil }
func (s stubStore) GetCurrentTraits() ([]memory.Trait, error)                      { return nil, nil }
func (s stubStore) GetPersonaState() (memory.PersonaState, error)                  { return memory.PersonaState{}, nil }
func (s stubStore) SetLastReflectionAt(t time.Time) error                          { return nil }
func (s stubStore) SetLastRewriteAt(t time.Time) error                             { return nil }
func (s stubStore) SetLastReflectedMessageID(id int64) error                       { return nil }
func (s stubStore) MessagesAfterID(afterID int64, limit int) ([]memory.Message, error) {
	return nil, nil
}
func (s stubStore) RecentReflections(limit int) ([]memory.Reflection, error)   { return nil, nil }
func (s stubStore) UnconsumedReflectionCount() (int, error)                     { return 0, nil }
func (s stubStore) GetTraitHistory(traitName string, limit int) ([]memory.Trait, error) {
	return nil, nil
}
func (s stubStore) SaveMoodEntry(entry *memory.MoodEntry) (int64, error)            { return 0, nil }
func (s stubStore) UpdateMoodEntry(id int64, entry *memory.MoodEntry) error         { return nil }
func (s stubStore) LatestMoodEntry(kind memory.MoodKind) (*memory.MoodEntry, error) { return nil, nil }
func (s stubStore) RecentMoodEntries(kind memory.MoodKind, limit int) ([]memory.MoodEntry, error) {
	return nil, nil
}
func (s stubStore) MoodEntriesInRange(kind memory.MoodKind, from, to time.Time) ([]memory.MoodEntry, error) {
	return nil, nil
}
func (s stubStore) SimilarMoodEntriesWithin(now time.Time, embedding []float32, window time.Duration, limit int) ([]memory.MoodEntry, error) {
	return nil, nil
}
func (s stubStore) DeleteMoodEntry(id int64) error                              { return nil }
func (s stubStore) SupersedeMoodEntry(oldID, newID int64, reason string) error  { return nil }
func (s stubStore) SavePendingMoodProposal(p *memory.PendingMoodProposal) (int64, error) {
	return 0, nil
}
func (s stubStore) PendingMoodProposalByMessageID(chatID, msgID int64) (*memory.PendingMoodProposal, error) {
	return nil, nil
}
func (s stubStore) DuePendingMoodProposals(now time.Time) ([]memory.PendingMoodProposal, error) {
	return nil, nil
}
func (s stubStore) UpdatePendingMoodProposalStatus(id int64, status memory.MoodProposalStatus) error {
	return nil
}
func (s stubStore) SaveMetric(model string, promptTokens, completionTokens, totalTokens int, costUSD float64, latencyMs int, messageID int64, isFallback bool) error {
	return nil
}
func (s stubStore) GetStats() (*memory.Stats, error)             { return nil, nil }
func (s stubStore) GetUsageReport() (*memory.UsageReport, error) { return nil, nil }
func (s stubStore) SaveSummary(conversationID, summary string, startID, endID int64, stream string) (int64, error) {
	return 0, nil
}
func (s stubStore) LatestSummary(conversationID, stream string) (string, int64, error) {
	return "", 0, nil
}
func (s stubStore) UpsertSchedulerTask(t *memory.SchedulerTask) error { return nil }
func (s stubStore) DueSchedulerTasks(now time.Time) ([]memory.SchedulerTask, error) {
	return nil, nil
}
func (s stubStore) SchedulerTaskByKind(kind string) (*memory.SchedulerTask, error) { return nil, nil }
func (s stubStore) MarkSchedulerSuccess(id int64, nextFire time.Time) error         { return nil }
func (s stubStore) MarkSchedulerFailure(id int64, nextFire time.Time, errMsg string, attempts int) error {
	return nil
}
func (s stubStore) DeleteSchedulerTask(id int64) error { return nil }
func (s stubStore) InsertCalendarEvent(title, start, end, location, notes, calendar, eventID, job string) (int64, error) {
	return 0, nil
}
func (s stubStore) UpdateCalendarEvent(id int64, updates map[string]any) error { return nil }
func (s stubStore) UpdateCalendarEventID(id int64, eventID string) error       { return nil }
func (s stubStore) DeleteCalendarEvent(id int64) error                         { return nil }
func (s stubStore) ListCalendarEvents(start, end, job string, shiftsOnly bool) ([]memory.CalendarEvent, error) {
	return nil, nil
}
func (s stubStore) GetCalendarEventByEventID(eventID string) (*memory.CalendarEvent, error) {
	return nil, nil
}
func (s stubStore) ListShiftEvents(start, end, job string) ([]memory.CalendarEvent, error) {
	return nil, nil
}
func (s stubStore) SavePIIVaultEntry(messageID int64, token, originalValue, entityType string) error {
	return nil
}
func (s stubStore) CreatePendingConfirmation(telegramMsgID int64, actionType string, actionPayload json.RawMessage, description string) (int64, error) {
	return 0, nil
}
func (s stubStore) GetPendingConfirmation(telegramMsgID int64) (*memory.PendingConfirmation, error) {
	return nil, nil
}
func (s stubStore) ResolvePendingConfirmation(id int64, action string) error { return nil }
func (s stubStore) InsertLocation(lat, lon float64, label, source, conversationID string) error {
	return nil
}
func (s stubStore) LatestLocation() *memory.LocationEntry { return nil }
func (s stubStore) SendInbox(sender, recipient, msgType, payload string) (int64, error) {
	return 0, nil
}
func (s stubStore) ConsumeInbox(recipient string) ([]memory.InboxMessage, error) { return nil, nil }
func (s stubStore) PendingInboxCount(recipient string) (int, error)               { return 0, nil }
func (s stubStore) GetEmbedDimension() int { return 0 }

// Memory Cards
func (s stubStore) GetCard(topicSlug string) (*memory.MemoryCard, error)   { return nil, nil }
func (s stubStore) GetCardByID(id int64) (*memory.MemoryCard, error)       { return nil, nil }
func (s stubStore) AllCards() ([]memory.MemoryCard, error)                 { return nil, nil }
func (s stubStore) CardsBySubject(subject string) ([]memory.MemoryCard, error) { return nil, nil }
func (s stubStore) UpdateCardSummary(topicSlug, newSummary, delta string, sourceMessageID int64) (*memory.MemoryCard, error) {
	return nil, nil
}
func (s stubStore) CreateCard(topicSlug, name, subject string, sourceMessageID int64) (*memory.MemoryCard, error) {
	return nil, nil
}
func (s stubStore) ExpireCard(topicSlug, reason string) error { return nil }
func (s stubStore) MergeCards(targetSlug, sourceSlug, mergedSummary, reason string) (*memory.MemoryCard, error) {
	return nil, nil
}
func (s stubStore) MemoriesByCard(cardID int64) ([]memory.Memory, error) { return nil, nil }
func (s stubStore) RecentLogEntries(hours int) ([]memory.MemoryLogEntry, error) { return nil, nil }
func (s stubStore) CardLogEntries(cardID int64, limit int) ([]memory.MemoryLogEntry, error) {
	return nil, nil
}
func (s stubStore) SemanticSearchByCard(queryVec []float32, cardID int64, topK int) ([]memory.Memory, error) {
	return nil, nil
}
func (s stubStore) SemanticSearchBySubject(queryVec []float32, subject string, topK int) ([]memory.Memory, error) {
	return nil, nil
}

// Ensure stubStore satisfies the full Store interface at compile time.
// This blank-identifier assignment is a Go idiom: if stubStore is
// missing any method, the compiler errors here rather than at the
// callsite — making the gap obvious and easy to find.
var _ memory.Store = stubStore{}

// newMinimalBot builds a Bot with only the fields getConversationID
// touches. The rest are left as zero values (nil pointers, empty
// maps, etc). We never call Start(), so the nil tb, cfg, etc.
// are fine — they're never dereferenced in this test path.
func newMinimalBot() *Bot {
	return &Bot{
		store: stubStore{},
		// conversationIDs and conversationIDsMu are value types inside
		// Bot, so they're automatically zero-initialised here — sync.Map
		// and sync.Mutex are both safe to use at their zero value.
	}
}

// TestGetConversationID_Concurrent verifies double-checked locking
// correctness: when 10 goroutines all call getConversationID for the
// same chatID simultaneously (with no cached value), they must all
// receive the identical string. The first goroutine to win the mutex
// creates the ID; everyone else must see it in the sync.Map fast-path
// on the re-check inside the lock.
//
// The real value of this test is running it with -race:
//   go test -race -run TestGetConversationID_Concurrent ./bot/
//
// Without the mutex fix, the race detector flags a concurrent write to
// the ID generation logic. With the fix, no races are reported.
func TestGetConversationID_Concurrent(t *testing.T) {
	b := newMinimalBot()
	const chatID = int64(123456789)
	const workers = 10

	results := make([]string, workers)
	var wg sync.WaitGroup
	wg.Add(workers)

	// Use a separate "start gate" so all goroutines begin at
	// the same moment rather than staggering. This maximises
	// the chance of actually hitting the race window.
	// In Python you'd do this with a threading.Barrier;
	// in Go we use a channel that we close all at once.
	startGate := make(chan struct{})

	for i := 0; i < workers; i++ {
		i := i // capture loop var — classic Go goroutine gotcha
		go func() {
			defer wg.Done()
			<-startGate // wait until all goroutines are ready
			results[i] = b.getConversationID(chatID)
		}()
	}

	// Release all goroutines simultaneously.
	close(startGate)
	wg.Wait()

	// All goroutines must have received the same ID.
	first := results[0]
	if first == "" {
		t.Fatal("getConversationID returned empty string")
	}
	for i, id := range results {
		if id != first {
			t.Errorf("goroutine %d got %q, want %q — non-deterministic ID creation detected", i, id, first)
		}
	}
}


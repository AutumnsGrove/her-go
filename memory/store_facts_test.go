package memory

import (
	"path/filepath"
	"testing"
)

func newFactTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "fact_test.db")
	store, err := NewStore(dbPath, 0)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func newFactTestStoreWithVec(t *testing.T, dim int) *SQLiteStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "fact_vec_test.db")
	store, err := NewStore(dbPath, dim)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// ---------- SaveMemory + GetMemory ----------

func TestSaveMemory_RoundTrip(t *testing.T) {
	store := newFactTestStore(t)

	id, err := store.SaveMemory(
		"Autumn likes hiking", "hobbies", "user",
		0, 7, nil, nil, "hiking,outdoors", "mentioned during weekend chat",
	)
	if err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}
	if id == 0 {
		t.Fatal("SaveMemory returned id=0")
	}

	m, err := store.GetMemory(id)
	if err != nil {
		t.Fatalf("GetMemory: %v", err)
	}
	if m == nil {
		t.Fatal("GetMemory returned nil")
	}
	if m.Content != "Autumn likes hiking" {
		t.Errorf("Content = %q, want %q", m.Content, "Autumn likes hiking")
	}
	if m.Category != "hobbies" {
		t.Errorf("Category = %q, want %q", m.Category, "hobbies")
	}
	if m.Subject != "user" {
		t.Errorf("Subject = %q, want %q", m.Subject, "user")
	}
	if m.Importance != 7 {
		t.Errorf("Importance = %d, want 7", m.Importance)
	}
	if m.Tags != "hiking,outdoors" {
		t.Errorf("Tags = %q, want %q", m.Tags, "hiking,outdoors")
	}
	if m.Context != "mentioned during weekend chat" {
		t.Errorf("Context = %q, want %q", m.Context, "mentioned during weekend chat")
	}
	if !m.Active {
		t.Error("memory should be active")
	}
}

func TestSaveMemory_SubjectDefaultsToUser(t *testing.T) {
	store := newFactTestStore(t)

	id, err := store.SaveMemory("test", "cat", "", 0, 5, nil, nil, "", "")
	if err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}
	m, _ := store.GetMemory(id)
	if m.Subject != "user" {
		t.Errorf("Subject = %q, want %q (default)", m.Subject, "user")
	}
}

func TestSaveMemory_SelfSubject(t *testing.T) {
	store := newFactTestStore(t)

	id, err := store.SaveMemory("I notice I use more metaphors at night", "meta", "self", 0, 5, nil, nil, "self-awareness", "")
	if err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}
	m, _ := store.GetMemory(id)
	if m.Subject != "self" {
		t.Errorf("Subject = %q, want %q", m.Subject, "self")
	}
}

func TestGetMemory_NotFound(t *testing.T) {
	store := newFactTestStore(t)

	m, err := store.GetMemory(99999)
	if err != nil {
		t.Fatalf("GetMemory error: %v", err)
	}
	if m != nil {
		t.Errorf("expected nil for nonexistent memory, got %+v", m)
	}
}

// ---------- UpdateMemory ----------

func TestUpdateMemory(t *testing.T) {
	store := newFactTestStore(t)

	id, _ := store.SaveMemory("old content", "old-cat", "user", 0, 3, nil, nil, "old-tags", "")
	err := store.UpdateMemory(id, "new content", "new-cat", 8, "new-tags")
	if err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}

	m, _ := store.GetMemory(id)
	if m.Content != "new content" {
		t.Errorf("Content = %q, want %q", m.Content, "new content")
	}
	if m.Category != "new-cat" {
		t.Errorf("Category = %q, want %q", m.Category, "new-cat")
	}
	if m.Importance != 8 {
		t.Errorf("Importance = %d, want 8", m.Importance)
	}
	if m.Tags != "new-tags" {
		t.Errorf("Tags = %q, want %q", m.Tags, "new-tags")
	}
}

func TestGetMemoryContent(t *testing.T) {
	store := newFactTestStore(t)

	id, _ := store.SaveMemory("Autumn is a barista", "work", "user", 0, 5, nil, nil, "", "")
	content, err := store.GetMemoryContent(id)
	if err != nil {
		t.Fatalf("GetMemoryContent: %v", err)
	}
	if content != "Autumn is a barista" {
		t.Errorf("content = %q, want %q", content, "Autumn is a barista")
	}
}

func TestGetMemoryContent_NotFound(t *testing.T) {
	store := newFactTestStore(t)

	content, err := store.GetMemoryContent(99999)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if content != "" {
		t.Errorf("expected empty string for nonexistent, got %q", content)
	}
}

// ---------- DeactivateMemory (soft delete) ----------

func TestDeactivateMemory(t *testing.T) {
	store := newFactTestStore(t)

	id, _ := store.SaveMemory("will be deleted", "test", "user", 0, 5, nil, nil, "", "")

	err := store.DeactivateMemory(id)
	if err != nil {
		t.Fatalf("DeactivateMemory: %v", err)
	}

	// GetMemory still finds it (includes inactive)
	m, _ := store.GetMemory(id)
	if m == nil {
		t.Fatal("GetMemory returned nil after deactivation")
	}
	if m.Active {
		t.Error("memory should be inactive after deactivation")
	}

	// But RecentMemories excludes it
	memories, _ := store.RecentMemories("user", 10)
	for _, mem := range memories {
		if mem.ID == id {
			t.Error("deactivated memory should not appear in RecentMemories")
		}
	}
}

// ---------- RecentMemories ----------

func TestRecentMemories_SubjectFilter(t *testing.T) {
	store := newFactTestStore(t)

	store.SaveMemory("user fact", "test", "user", 0, 5, nil, nil, "", "")
	store.SaveMemory("self fact", "test", "self", 0, 5, nil, nil, "", "")
	store.SaveMemory("another user fact", "test", "user", 0, 5, nil, nil, "", "")

	userMems, _ := store.RecentMemories("user", 10)
	if len(userMems) != 2 {
		t.Errorf("got %d user memories, want 2", len(userMems))
	}

	selfMems, _ := store.RecentMemories("self", 10)
	if len(selfMems) != 1 {
		t.Errorf("got %d self memories, want 1", len(selfMems))
	}
}

func TestRecentMemories_RespectsLimit(t *testing.T) {
	store := newFactTestStore(t)

	store.SaveMemory("one", "test", "user", 0, 5, nil, nil, "", "")
	store.SaveMemory("two", "test", "user", 0, 5, nil, nil, "", "")
	store.SaveMemory("three", "test", "user", 0, 5, nil, nil, "", "")

	mems, _ := store.RecentMemories("user", 2)
	if len(mems) != 2 {
		t.Fatalf("got %d memories, want 2 (limit should cap results)", len(mems))
	}
}

// ---------- AllActiveMemories ----------

func TestAllActiveMemories(t *testing.T) {
	store := newFactTestStore(t)

	store.SaveMemory("active user", "test", "user", 0, 5, nil, nil, "", "")
	id2, _ := store.SaveMemory("will deactivate", "test", "user", 0, 5, nil, nil, "", "")
	store.SaveMemory("active self", "test", "self", 0, 5, nil, nil, "", "")
	store.DeactivateMemory(id2)

	all, err := store.AllActiveMemories()
	if err != nil {
		t.Fatalf("AllActiveMemories: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("got %d active memories, want 2", len(all))
	}
}

// ---------- FindMemoriesByKeyword ----------

func TestFindMemoriesByKeyword(t *testing.T) {
	store := newFactTestStore(t)

	store.SaveMemory("Autumn likes hiking in the mountains", "hobbies", "user", 0, 5, nil, nil, "", "")
	store.SaveMemory("Autumn works at a coffee shop", "work", "user", 0, 5, nil, nil, "", "")
	store.SaveMemory("deactivated hiking memory", "hobbies", "user", 0, 5, nil, nil, "", "")
	// Deactivate the last one
	mems, _ := store.RecentMemories("user", 1)
	store.DeactivateMemory(mems[0].ID)

	results, err := store.FindMemoriesByKeyword("hiking")
	if err != nil {
		t.Fatalf("FindMemoriesByKeyword: %v", err)
	}
	// Only the active hiking memory should match
	if len(results) != 1 {
		t.Errorf("got %d results, want 1 (deactivated should be excluded)", len(results))
	}
}

// ---------- Supersession ----------

func TestSupersedeMemory(t *testing.T) {
	store := newFactTestStore(t)

	oldID, _ := store.SaveMemory("works at Panera", "work", "user", 0, 5, nil, nil, "", "")
	newID, _ := store.SaveMemory("works at Starbucks", "work", "user", 0, 5, nil, nil, "", "")

	err := store.SupersedeMemory(oldID, newID, "job changed")
	if err != nil {
		t.Fatalf("SupersedeMemory: %v", err)
	}

	old, _ := store.GetMemory(oldID)
	if old.Active {
		t.Error("superseded memory should be inactive")
	}
	if old.SupersededBy != newID {
		t.Errorf("SupersededBy = %d, want %d", old.SupersededBy, newID)
	}
	if old.SupersedeReason != "job changed" {
		t.Errorf("SupersedeReason = %q, want %q", old.SupersedeReason, "job changed")
	}

	new, _ := store.GetMemory(newID)
	if !new.Active {
		t.Error("replacement memory should still be active")
	}
}

func TestMemoryHistory_Chain(t *testing.T) {
	store := newFactTestStore(t)

	id1, _ := store.SaveMemory("v1: works at McDonald's", "work", "user", 0, 5, nil, nil, "", "")
	id2, _ := store.SaveMemory("v2: works at Panera", "work", "user", 0, 5, nil, nil, "", "")
	id3, _ := store.SaveMemory("v3: works at Starbucks", "work", "user", 0, 5, nil, nil, "", "")

	store.SupersedeMemory(id1, id2, "job changed")
	store.SupersedeMemory(id2, id3, "job changed again")

	// Query history from the middle — should get all 3
	chain, err := store.MemoryHistory(id2)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	if len(chain) != 3 {
		t.Fatalf("chain length = %d, want 3", len(chain))
	}
	// oldest first
	if chain[0].ID != id1 {
		t.Errorf("chain[0].ID = %d, want %d (oldest)", chain[0].ID, id1)
	}
	if chain[2].ID != id3 {
		t.Errorf("chain[2].ID = %d, want %d (newest)", chain[2].ID, id3)
	}
}

// ---------- Zettelkasten linking ----------

func TestLinkMemories_RoundTrip(t *testing.T) {
	store := newFactTestStore(t)

	id1, _ := store.SaveMemory("likes hiking", "hobbies", "user", 0, 5, nil, nil, "", "")
	id2, _ := store.SaveMemory("enjoys being outdoors", "hobbies", "user", 0, 5, nil, nil, "", "")

	err := store.LinkMemories(id1, id2, 0.85)
	if err != nil {
		t.Fatalf("LinkMemories: %v", err)
	}

	// Query linked memories from id1
	linked, err := store.LinkedMemories(id1, 10)
	if err != nil {
		t.Fatalf("LinkedMemories: %v", err)
	}
	if len(linked) != 1 {
		t.Fatalf("got %d linked memories, want 1", len(linked))
	}
	if linked[0].ID != id2 {
		t.Errorf("linked memory ID = %d, want %d", linked[0].ID, id2)
	}
	if linked[0].Source != "linked" {
		t.Errorf("Source = %q, want %q", linked[0].Source, "linked")
	}
}

func TestLinkMemories_Bidirectional(t *testing.T) {
	store := newFactTestStore(t)

	id1, _ := store.SaveMemory("fact A", "test", "user", 0, 5, nil, nil, "", "")
	id2, _ := store.SaveMemory("fact B", "test", "user", 0, 5, nil, nil, "", "")

	store.LinkMemories(id1, id2, 0.9)

	// Should be visible from both sides
	fromA, _ := store.LinkedMemories(id1, 10)
	fromB, _ := store.LinkedMemories(id2, 10)

	if len(fromA) != 1 || fromA[0].ID != id2 {
		t.Errorf("link not visible from id1")
	}
	if len(fromB) != 1 || fromB[0].ID != id1 {
		t.Errorf("link not visible from id2")
	}
}

func TestLinkMemories_DuplicateIsNoop(t *testing.T) {
	store := newFactTestStore(t)

	id1, _ := store.SaveMemory("A", "test", "user", 0, 5, nil, nil, "", "")
	id2, _ := store.SaveMemory("B", "test", "user", 0, 5, nil, nil, "", "")

	store.LinkMemories(id1, id2, 0.85)
	// Linking again (even reversed) should not error or create a duplicate
	err := store.LinkMemories(id2, id1, 0.90)
	if err != nil {
		t.Fatalf("duplicate LinkMemories should not error: %v", err)
	}

	count, _ := store.CountMemoryLinks()
	if count != 1 {
		t.Errorf("got %d links, want 1 (duplicate should be ignored)", count)
	}
}

func TestLinkedMemories_ExcludesInactive(t *testing.T) {
	store := newFactTestStore(t)

	id1, _ := store.SaveMemory("active", "test", "user", 0, 5, nil, nil, "", "")
	id2, _ := store.SaveMemory("will deactivate", "test", "user", 0, 5, nil, nil, "", "")

	store.LinkMemories(id1, id2, 0.8)
	store.DeactivateMemory(id2)

	linked, _ := store.LinkedMemories(id1, 10)
	if len(linked) != 0 {
		t.Errorf("got %d linked memories, want 0 (deactivated should be excluded)", len(linked))
	}
}

// ---------- Embeddings with vec_memories ----------

func TestSaveMemory_WithEmbedding(t *testing.T) {
	store := newFactTestStoreWithVec(t, 4) // 4-dim embeddings for testing

	emb := []float32{0.1, 0.2, 0.3, 0.4}
	embText := []float32{0.5, 0.6, 0.7, 0.8}

	id, err := store.SaveMemory("test fact", "test", "user", 0, 5, emb, embText, "tags", "")
	if err != nil {
		t.Fatalf("SaveMemory with embedding: %v", err)
	}

	m, _ := store.GetMemory(id)
	if m == nil {
		t.Fatal("GetMemory returned nil")
	}

	// Verify vec_memories has the entry
	count, err := store.VecMemoriesCount()
	if err != nil {
		t.Fatalf("VecMemoriesCount: %v", err)
	}
	if count != 1 {
		t.Errorf("vec_memories count = %d, want 1", count)
	}
}

func TestSemanticSearch_BasicKNN(t *testing.T) {
	store := newFactTestStoreWithVec(t, 4)

	// Save a few memories with different embeddings
	store.SaveMemory("about hiking", "hobbies", "user", 0, 5,
		[]float32{1.0, 0.0, 0.0, 0.0}, nil, "hiking", "")
	store.SaveMemory("about cooking", "hobbies", "user", 0, 5,
		[]float32{0.0, 1.0, 0.0, 0.0}, nil, "cooking", "")
	store.SaveMemory("about coding", "work", "user", 0, 5,
		[]float32{0.0, 0.0, 1.0, 0.0}, nil, "coding", "")

	// Search with a vector close to "hiking"
	results, err := store.SemanticSearch([]float32{0.9, 0.1, 0.0, 0.0}, 2)
	if err != nil {
		t.Fatalf("SemanticSearch: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("SemanticSearch returned no results")
	}
	// Nearest result should be the hiking memory
	if results[0].Content != "about hiking" {
		t.Errorf("closest match = %q, want %q", results[0].Content, "about hiking")
	}
	if results[0].Source != "semantic" {
		t.Errorf("Source = %q, want %q", results[0].Source, "semantic")
	}
}

func TestSemanticSearch_ExcludesInactive(t *testing.T) {
	store := newFactTestStoreWithVec(t, 4)

	id, _ := store.SaveMemory("will be deactivated", "test", "user", 0, 5,
		[]float32{1.0, 0.0, 0.0, 0.0}, nil, "test", "")
	store.DeactivateMemory(id)

	results, err := store.SemanticSearch([]float32{1.0, 0.0, 0.0, 0.0}, 5)
	if err != nil {
		t.Fatalf("SemanticSearch: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("got %d results, want 0 (deactivated memory should be excluded)", len(results))
	}
}

func TestMemoriesWithoutEmbeddings(t *testing.T) {
	store := newFactTestStore(t)

	// One with embedding, one without
	store.SaveMemory("has embedding", "test", "user", 0, 5, []float32{0.1, 0.2}, nil, "", "")
	store.SaveMemory("no embedding", "test", "user", 0, 5, nil, nil, "", "")

	mems, err := store.MemoriesWithoutEmbeddings()
	if err != nil {
		t.Fatalf("MemoriesWithoutEmbeddings: %v", err)
	}
	if len(mems) != 1 {
		t.Fatalf("got %d memories without embeddings, want 1", len(mems))
	}
	if mems[0].Content != "no embedding" {
		t.Errorf("content = %q, want %q", mems[0].Content, "no embedding")
	}
}

func TestUpdateMemoryEmbedding(t *testing.T) {
	store := newFactTestStoreWithVec(t, 4)

	// Save without embedding first
	id, _ := store.SaveMemory("test", "test", "user", 0, 5, nil, nil, "", "")

	emb := []float32{0.1, 0.2, 0.3, 0.4}
	embText := []float32{0.5, 0.6, 0.7, 0.8}

	err := store.UpdateMemoryEmbedding(id, emb, embText)
	if err != nil {
		t.Fatalf("UpdateMemoryEmbedding: %v", err)
	}

	// Should now be searchable
	count, _ := store.VecMemoriesCount()
	if count != 1 {
		t.Errorf("vec_memories count = %d, want 1 after embedding update", count)
	}
}

// ---------- Embedding serialization ----------

func TestSerializeDeserializeEmbedding(t *testing.T) {
	original := []float32{0.1, -0.5, 3.14, 0.0}

	bytes, err := serializeEmbedding(original)
	if err != nil {
		t.Fatalf("serializeEmbedding: %v", err)
	}
	if len(bytes) != 16 { // 4 floats * 4 bytes each
		t.Errorf("serialized length = %d, want 16", len(bytes))
	}

	restored := deserializeEmbedding(bytes)
	if len(restored) != len(original) {
		t.Fatalf("restored length = %d, want %d", len(restored), len(original))
	}
	for i := range original {
		if restored[i] != original[i] {
			t.Errorf("restored[%d] = %f, want %f", i, restored[i], original[i])
		}
	}
}

func TestDeserializeEmbedding_Empty(t *testing.T) {
	result := deserializeEmbedding(nil)
	if result != nil {
		t.Errorf("expected nil for nil input, got %v", result)
	}

	result = deserializeEmbedding([]byte{})
	if result != nil {
		t.Errorf("expected nil for empty input, got %v", result)
	}
}

func TestDeserializeEmbedding_BadLength(t *testing.T) {
	// 5 bytes is not a multiple of 4 (float32 size)
	result := deserializeEmbedding([]byte{1, 2, 3, 4, 5})
	if result != nil {
		t.Errorf("expected nil for non-aligned input, got %v", result)
	}
}

package persona

import (
	"testing"

	"her/memory"
)

// makeMemory builds a test memory with a fake embedding vector.
// The vector is a unit vector in the given dimension (1.0 at index dim, 0 elsewhere).
// Memories with the same dim will have cosine similarity 1.0; different dims → 0.0.
func makeMemory(id int64, content string, dim int) memory.Memory {
	vec := make([]float32, 8)
	if dim >= 0 && dim < len(vec) {
		vec[dim] = 1.0
	}
	return memory.Memory{
		ID:            id,
		Content:       content,
		Category:      "test",
		Subject:       "user",
		EmbeddingText: vec,
	}
}

func TestClusterMemories_Empty(t *testing.T) {
	clusters, lonely := ClusterMemories(nil, 0.7)
	if len(clusters) != 0 || len(lonely) != 0 {
		t.Errorf("expected empty results, got %d clusters, %d lonely", len(clusters), len(lonely))
	}
}

func TestClusterMemories_AllUnique(t *testing.T) {
	mems := []memory.Memory{
		makeMemory(1, "fact A", 0),
		makeMemory(2, "fact B", 1),
		makeMemory(3, "fact C", 2),
	}
	clusters, lonely := ClusterMemories(mems, 0.7)
	if len(clusters) != 0 {
		t.Errorf("expected 0 clusters, got %d", len(clusters))
	}
	if len(lonely) != 3 {
		t.Errorf("expected 3 lonely, got %d", len(lonely))
	}
}

func TestClusterMemories_AllIdentical(t *testing.T) {
	mems := []memory.Memory{
		makeMemory(1, "fact A", 0),
		makeMemory(2, "fact A copy", 0),
		makeMemory(3, "fact A again", 0),
	}
	clusters, lonely := ClusterMemories(mems, 0.7)
	if len(clusters) != 1 {
		t.Errorf("expected 1 cluster, got %d", len(clusters))
	}
	if len(clusters) > 0 && len(clusters[0].Memories) != 3 {
		t.Errorf("expected cluster of 3, got %d", len(clusters[0].Memories))
	}
	if len(lonely) != 0 {
		t.Errorf("expected 0 lonely, got %d", len(lonely))
	}
}

func TestClusterMemories_MixedClusters(t *testing.T) {
	mems := []memory.Memory{
		makeMemory(1, "sobriety A", 0),
		makeMemory(2, "sobriety B", 0),
		makeMemory(3, "debt A", 1),
		makeMemory(4, "debt B", 1),
		makeMemory(5, "lonely fact", 2),
	}
	clusters, lonely := ClusterMemories(mems, 0.7)
	if len(clusters) != 2 {
		t.Errorf("expected 2 clusters, got %d", len(clusters))
	}
	if len(lonely) != 1 {
		t.Errorf("expected 1 lonely, got %d", len(lonely))
	}
	if len(lonely) > 0 && lonely[0].ID != 5 {
		t.Errorf("expected lonely ID=5, got ID=%d", lonely[0].ID)
	}
}

func TestClusterMemories_SkipsNoEmbedding(t *testing.T) {
	mems := []memory.Memory{
		makeMemory(1, "has embedding", 0),
		{ID: 2, Content: "no embedding", Category: "test", Subject: "user"},
		makeMemory(3, "also has embedding", 0),
	}
	clusters, lonely := ClusterMemories(mems, 0.7)
	if len(clusters) != 1 {
		t.Errorf("expected 1 cluster, got %d", len(clusters))
	}
	if len(lonely) != 1 {
		t.Errorf("expected 1 lonely (the one without embedding), got %d", len(lonely))
	}
}

func TestClusterMemories_ThresholdBoundary(t *testing.T) {
	// Two vectors that are similar but not identical.
	// [0.8, 0.6, 0, ...] and [0.6, 0.8, 0, ...] have cosine similarity ≈ 0.96
	vec1 := make([]float32, 8)
	vec1[0], vec1[1] = 0.8, 0.6
	vec2 := make([]float32, 8)
	vec2[0], vec2[1] = 0.6, 0.8

	mems := []memory.Memory{
		{ID: 1, Content: "similar A", EmbeddingText: vec1},
		{ID: 2, Content: "similar B", EmbeddingText: vec2},
	}

	// Should cluster at 0.9 threshold (sim ≈ 0.96)
	clusters, _ := ClusterMemories(mems, 0.9)
	if len(clusters) != 1 {
		t.Errorf("expected 1 cluster at threshold 0.9, got %d", len(clusters))
	}

	// Should NOT cluster at 0.99 threshold
	clusters, lonely := ClusterMemories(mems, 0.99)
	if len(clusters) != 0 {
		t.Errorf("expected 0 clusters at threshold 0.99, got %d", len(clusters))
	}
	if len(lonely) != 2 {
		t.Errorf("expected 2 lonely at threshold 0.99, got %d", len(lonely))
	}
}

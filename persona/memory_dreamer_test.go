package persona

import (
	"strings"
	"testing"
	"time"

	"her/memory"
)

func TestBuildDreamerTranscript_ClustersAndLonely(t *testing.T) {
	now := time.Now()
	clusters := []MemoryCluster{
		{Memories: []memory.Memory{
			{ID: 1, Content: "sobriety A", Category: "health", Importance: 5, Subject: "user", Timestamp: now.Add(-48 * time.Hour)},
			{ID: 2, Content: "sobriety B", Category: "health", Importance: 5, Subject: "user", Timestamp: now.Add(-24 * time.Hour)},
		}},
	}
	lonely := []memory.Memory{
		{ID: 10, Content: "lonely fact", Category: "mood", Importance: 9, Subject: "user", Timestamp: now.Add(-72 * time.Hour)},
	}

	result := buildDreamerTranscript(clusters, lonely)

	if !strings.Contains(result, "Cluster 1 (2 memories)") {
		t.Error("missing cluster header")
	}
	if !strings.Contains(result, "ID=1") && !strings.Contains(result, "sobriety A") {
		t.Error("missing cluster memory 1")
	}
	if !strings.Contains(result, "ID=2") && !strings.Contains(result, "sobriety B") {
		t.Error("missing cluster memory 2")
	}
	if !strings.Contains(result, "Lonely memories") {
		t.Error("missing lonely section")
	}
	if !strings.Contains(result, "ID=10") {
		t.Error("missing lonely memory")
	}
	if !strings.Contains(result, "cat=mood") {
		t.Error("missing category in lonely memory")
	}
}

func TestBuildDreamerTranscript_Empty(t *testing.T) {
	result := buildDreamerTranscript(nil, nil)

	if !strings.Contains(result, "Memory Consolidation Review") {
		t.Error("missing header")
	}
	if strings.Contains(result, "Cluster") {
		t.Error("should have no clusters")
	}
	if strings.Contains(result, "Lonely") {
		t.Error("should have no lonely section")
	}
}

func TestBuildDreamerTranscript_AgeCalculation(t *testing.T) {
	clusters := []MemoryCluster{
		{Memories: []memory.Memory{
			{ID: 1, Content: "fact", Category: "test", Importance: 5, Subject: "user", Timestamp: time.Now().Add(-7 * 24 * time.Hour)},
		}},
	}

	result := buildDreamerTranscript(clusters, nil)

	// Should show ~7 days age.
	if !strings.Contains(result, "age=7d") {
		t.Errorf("expected age=7d in output, got:\n%s", result)
	}
}

package persona

import (
	"her/embed"
	"her/memory"
)

// MemoryCluster groups memories that are semantically similar enough
// to warrant review by the memory dreamer. The agent decides whether
// to merge, split, or leave them as-is.
type MemoryCluster struct {
	Memories []memory.Memory
}

// ClusterMemories groups active memories by embedding similarity using
// a union-find (disjoint set) algorithm. Memories whose cosine similarity
// exceeds the threshold are placed in the same cluster.
//
// Returns two slices:
//   - clusters: groups of 2+ memories that are semantically close (merge candidates)
//   - lonely: memories that don't cluster with anything (staleness review candidates)
//
// Union-find is a classic algorithm for grouping connected components.
// Think of it like Python's networkx.connected_components() but in ~30 lines.
// Each element starts as its own parent. When two elements are similar,
// we "union" their sets. Path compression (find chasing up to the root)
// keeps lookups nearly O(1).
func ClusterMemories(memories []memory.Memory, threshold float64) (clusters []MemoryCluster, lonely []memory.Memory) {
	n := len(memories)
	if n == 0 {
		return nil, nil
	}

	// Union-find parent array. parent[i] = i means "I am my own root".
	parent := make([]int, n)
	for i := range parent {
		parent[i] = i
	}

	// find with path compression — chases parent pointers to the root,
	// then flattens the path so future lookups are O(1).
	var find func(int) int
	find = func(x int) int {
		if parent[x] != x {
			parent[x] = find(parent[x])
		}
		return parent[x]
	}

	// union merges two sets by pointing one root to the other.
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[ra] = rb
		}
	}

	// Compare all pairs. At 100 memories with 768-dim vectors this is
	// ~5000 cosine similarity computations — sub-millisecond in Go.
	for i := 0; i < n; i++ {
		if len(memories[i].Embedding) == 0 && len(memories[i].EmbeddingText) == 0 {
			continue
		}
		vecI := memories[i].EmbeddingText
		if len(vecI) == 0 {
			vecI = memories[i].Embedding
		}

		for j := i + 1; j < n; j++ {
			if len(memories[j].Embedding) == 0 && len(memories[j].EmbeddingText) == 0 {
				continue
			}
			vecJ := memories[j].EmbeddingText
			if len(vecJ) == 0 {
				vecJ = memories[j].Embedding
			}

			sim := embed.CosineSimilarity(vecI, vecJ)
			if sim >= threshold {
				union(i, j)
			}
		}
	}

	// Group by root.
	groups := make(map[int][]int)
	for i := 0; i < n; i++ {
		root := find(i)
		groups[root] = append(groups[root], i)
	}

	for _, indices := range groups {
		if len(indices) == 1 {
			lonely = append(lonely, memories[indices[0]])
		} else {
			c := MemoryCluster{Memories: make([]memory.Memory, len(indices))}
			for i, idx := range indices {
				c.Memories[i] = memories[idx]
			}
			clusters = append(clusters, c)
		}
	}

	return clusters, lonely
}

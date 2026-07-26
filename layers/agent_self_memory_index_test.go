package layers

import (
	"strings"
	"testing"
)

// TestBuildSelfMemoryIndex_EmptyStoreEmitsNothing verifies the layer stays
// silent (no content, no token overhead) when there are no self-memories yet.
func TestBuildSelfMemoryIndex_EmptyStoreEmitsNothing(t *testing.T) {
	store := newLayerTestStore(t)
	res := buildSelfMemoryIndex(&LayerContext{Store: store})
	if res.Content != "" {
		t.Errorf("Content = %q, want empty for a store with no self-memories", res.Content)
	}
}

// TestBuildSelfMemoryIndex_ShowsHookAndSlug verifies a self-memory with both
// a hook and a slug renders both, and that a user-subject memory in the same
// store is excluded (this layer is self-only by design).
func TestBuildSelfMemoryIndex_ShowsHookAndSlug(t *testing.T) {
	store := newLayerTestStore(t)

	_, err := store.SaveMemory("Mira tends to over-hedge critical feedback", "pattern", "self",
		0, 5, nil, nil, "", "", 0, "say it straight when asked for honesty", "over-hedges-feedback")
	if err != nil {
		t.Fatalf("SaveMemory (self): %v", err)
	}
	_, err = store.SaveMemory("Autumn has a dog named Biscuit", "identity", "user",
		0, 5, nil, nil, "", "", 0, "has a dog named Biscuit", "dog-biscuit")
	if err != nil {
		t.Fatalf("SaveMemory (user): %v", err)
	}

	res := buildSelfMemoryIndex(&LayerContext{Store: store})

	if !strings.Contains(res.Content, "over-hedges-feedback") {
		t.Errorf("Content = %q, want the self-memory's slug", res.Content)
	}
	if !strings.Contains(res.Content, "say it straight when asked for honesty") {
		t.Errorf("Content = %q, want the self-memory's hook", res.Content)
	}
	if strings.Contains(res.Content, "Biscuit") || strings.Contains(res.Content, "dog-biscuit") {
		t.Errorf("Content = %q, want the user-subject memory excluded", res.Content)
	}
	if res.Detail != "1 self-memories" {
		t.Errorf("Detail = %q, want %q", res.Detail, "1 self-memories")
	}
}

// TestBuildSelfMemoryIndex_MissingHookFallsBack verifies a self-memory saved
// without a hook (e.g. pre-backfill, or backfill failed) still shows up in
// the index rather than being silently dropped.
func TestBuildSelfMemoryIndex_MissingHookFallsBack(t *testing.T) {
	store := newLayerTestStore(t)

	_, err := store.SaveMemory("I notice small emotional shifts in phrasing", "pattern", "self",
		0, 5, nil, nil, "", "", 0, "", "") // no hook, no slug
	if err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}

	res := buildSelfMemoryIndex(&LayerContext{Store: store})

	if !strings.Contains(res.Content, "(no hook yet)") {
		t.Errorf("Content = %q, want the no-hook fallback text", res.Content)
	}
}

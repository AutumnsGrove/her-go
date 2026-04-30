package mood

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"her/memory"
)

// pngMagic is the first 8 bytes of any valid PNG — checking these
// bytes in the render output is a lightweight "did it actually emit
// a PNG" assertion without embedding a full decoder in the test.
var pngMagic = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}

func newGraphTestStore(t *testing.T) memory.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "graph.db")
	store, err := memory.NewStore(dbPath, 0)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestRenderValencePNG_ProducesValidPNGBytes(t *testing.T) {
	store := newGraphTestStore(t)
	now := time.Now()
	// Seed one entry per day for 5 days.
	for i := 0; i < 5; i++ {
		_, err := store.SaveMoodEntry(&memory.MoodEntry{
			Kind:    memory.MoodKindMomentary,
			Valence: 3 + i%3,
			Labels:  []string{"Calm"},
			Source:  memory.MoodSourceInferred,
			Timestamp: now.Add(-time.Duration(i) * 24 * time.Hour),
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	png, err := RenderValencePNG(store, Default(), GraphRangeWeek, now)
	if err != nil {
		t.Fatalf("RenderValencePNG: %v", err)
	}
	if len(png) < 100 {
		t.Errorf("png byte length = %d, suspiciously small", len(png))
	}
	if !bytes.HasPrefix(png, pngMagic) {
		t.Errorf("output doesn't start with PNG magic bytes: %x", png[:8])
	}
}

func TestRenderValencePNG_EmptyStoreReturnsFallbackPNG(t *testing.T) {
	store := newGraphTestStore(t)
	png, err := RenderValencePNG(store, Default(), GraphRangeMonth, time.Now())
	if err != nil {
		t.Fatalf("RenderValencePNG (empty): %v", err)
	}
	if !bytes.HasPrefix(png, pngMagic) {
		t.Error("empty-fallback output isn't a valid PNG")
	}
}

func TestRenderValencePNG_NilVocabUsesDefault(t *testing.T) {
	store := newGraphTestStore(t)
	_, err := store.SaveMoodEntry(&memory.MoodEntry{
		Kind: memory.MoodKindMomentary, Valence: 5, Labels: []string{"Happy"},
	})
	if err != nil {
		t.Fatal(err)
	}

	png, err := RenderValencePNG(store, nil, GraphRangeWeek, time.Now())
	if err != nil {
		t.Fatalf("RenderValencePNG (nil vocab): %v", err)
	}
	if !bytes.HasPrefix(png, pngMagic) {
		t.Error("output isn't a valid PNG")
	}
}

func TestGraphRangeDurations(t *testing.T) {
	if GraphRangeWeek.Duration() != 7*24*time.Hour {
		t.Errorf("week = %v, want 7d", GraphRangeWeek.Duration())
	}
	if GraphRangeMonth.Duration() != 30*24*time.Hour {
		t.Errorf("month = %v, want 30d", GraphRangeMonth.Duration())
	}
	if GraphRangeYear.Duration() != 365*24*time.Hour {
		t.Errorf("year = %v, want 365d", GraphRangeYear.Duration())
	}
}

func TestParseHex(t *testing.T) {
	got := parseHex("#F68B22")
	if got.R != 0xF6 || got.G != 0x8B || got.B != 0x22 {
		t.Errorf("parseHex(#F68B22) = %v, want (0xF6, 0x8B, 0x22)", got)
	}
	if got.A != 255 {
		t.Errorf("alpha = %d, want 255", got.A)
	}

	// Malformed input → black (not a panic).
	black := parseHex("tomato")
	if black.R != 0 || black.G != 0 || black.B != 0 {
		t.Errorf("malformed hex = %v, want black", black)
	}
}

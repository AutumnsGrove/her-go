package loader

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testEmbedDim is a small dimension for test embeddings.
// Real embeddings are 768-dimensional, but for tests we only need a few
// floats to verify KNN search works.
const testEmbedDim = 4

// makeTestEmbedding creates a simple test vector. The values are
// arbitrary — what matters is that similar vectors return higher
// cosine similarity scores than dissimilar ones.
func makeTestEmbedding(vals ...float32) []float32 {
	vec := make([]float32, testEmbedDim)
	copy(vec, vals)
	return vec
}

func TestSidecarRecordAndSearch(t *testing.T) {
	dir := t.TempDir()

	skill := &Skill{
		Name:       "test_skill",
		Language:   "go",
		Dir:        dir,
		TrustLevel: TrustSecondParty,
	}

	sdb, err := OpenSidecar(skill, testEmbedDim)
	if err != nil {
		t.Fatalf("OpenSidecar() error: %v", err)
	}
	defer sdb.Close()

	// Record a run with a known embedding.
	args := map[string]any{"query": "cats"}
	result := &RunResult{
		Output:   json.RawMessage(`{"answer": "cats are great"}`),
		Duration: 150 * time.Millisecond,
	}
	embedding := makeTestEmbedding(1.0, 0.0, 0.0, 0.0)

	if err := sdb.RecordRun(args, result, embedding); err != nil {
		t.Fatalf("RecordRun() error: %v", err)
	}

	// Search with the same vector — should find our run.
	results, err := sdb.SearchHistory(embedding, 5)
	if err != nil {
		t.Fatalf("SearchHistory() error: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	r := results[0]
	if r.ExitCode != 0 {
		t.Errorf("expected exit_code 0, got %d", r.ExitCode)
	}
	if r.Args != `{"query":"cats"}` {
		t.Errorf("args = %q, want {\"query\":\"cats\"}", r.Args)
	}
	if r.Result != `{"answer": "cats are great"}` {
		t.Errorf("result = %q, unexpected", r.Result)
	}
	if r.Age == "" {
		t.Error("expected non-empty age string")
	}
}

func TestSidecarEmptySearch(t *testing.T) {
	dir := t.TempDir()

	skill := &Skill{
		Name:       "empty_skill",
		Language:   "go",
		Dir:        dir,
		TrustLevel: TrustSecondParty,
	}

	sdb, err := OpenSidecar(skill, testEmbedDim)
	if err != nil {
		t.Fatalf("OpenSidecar() error: %v", err)
	}
	defer sdb.Close()

	// Search on empty DB — should return empty slice, not error.
	queryVec := makeTestEmbedding(1.0, 0.0, 0.0, 0.0)
	results, err := sdb.SearchHistory(queryVec, 5)
	if err != nil {
		t.Fatalf("SearchHistory() on empty DB error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results on empty DB, got %d", len(results))
	}
}

func TestSidecarMultipleRuns(t *testing.T) {
	dir := t.TempDir()

	skill := &Skill{
		Name:       "multi_skill",
		Language:   "go",
		Dir:        dir,
		TrustLevel: TrustSecondParty,
	}

	sdb, err := OpenSidecar(skill, testEmbedDim)
	if err != nil {
		t.Fatalf("OpenSidecar() error: %v", err)
	}
	defer sdb.Close()

	// Record three runs with different embeddings.
	runs := []struct {
		query     string
		embedding []float32
	}{
		{"cats", makeTestEmbedding(1.0, 0.0, 0.0, 0.0)},
		{"dogs", makeTestEmbedding(0.0, 1.0, 0.0, 0.0)},
		{"fish", makeTestEmbedding(0.0, 0.0, 1.0, 0.0)},
	}

	for _, run := range runs {
		args := map[string]any{"query": run.query}
		result := &RunResult{
			Output:   json.RawMessage(`{"answer": "` + run.query + ` info"}`),
			Duration: 100 * time.Millisecond,
		}
		if err := sdb.RecordRun(args, result, run.embedding); err != nil {
			t.Fatalf("RecordRun(%s) error: %v", run.query, err)
		}
	}

	// Search for "cats" — should return cats first (closest match).
	queryVec := makeTestEmbedding(1.0, 0.0, 0.0, 0.0)
	results, err := sdb.SearchHistory(queryVec, 3)
	if err != nil {
		t.Fatalf("SearchHistory() error: %v", err)
	}

	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}

	// First result should be "cats" (exact match, distance ≈ 0).
	if results[0].Args != `{"query":"cats"}` {
		t.Errorf("expected cats as first result, got %q", results[0].Args)
	}
}

func TestSidecarFourthPartyBlocked(t *testing.T) {
	skill := &Skill{
		Name:       "untrusted_skill",
		Language:   "go",
		Dir:        t.TempDir(),
		TrustLevel: TrustFourthParty,
	}

	_, err := OpenSidecar(skill, testEmbedDim)
	if err == nil {
		t.Fatal("expected error for 4th-party skill, got nil")
	}
}

func TestSidecarThirdPartyCanOpen(t *testing.T) {
	// 3rd-party skills CAN open the sidecar (for read access).
	// Write restriction is enforced at the call site (runner.go).
	skill := &Skill{
		Name:       "modified_skill",
		Language:   "go",
		Dir:        t.TempDir(),
		TrustLevel: TrustThirdParty,
	}

	sdb, err := OpenSidecar(skill, testEmbedDim)
	if err != nil {
		t.Fatalf("expected 3rd-party to open sidecar, got error: %v", err)
	}
	sdb.Close()
}

func TestSidecarErrorResult(t *testing.T) {
	dir := t.TempDir()

	skill := &Skill{
		Name:       "error_skill",
		Language:   "go",
		Dir:        dir,
		TrustLevel: TrustSecondParty,
	}

	sdb, err := OpenSidecar(skill, testEmbedDim)
	if err != nil {
		t.Fatalf("OpenSidecar() error: %v", err)
	}
	defer sdb.Close()

	// Record a failed run.
	args := map[string]any{"query": "fail"}
	result := &RunResult{
		Error:    "skill timed out after 5s",
		Duration: 5 * time.Second,
	}
	embedding := makeTestEmbedding(0.5, 0.5, 0.0, 0.0)

	if err := sdb.RecordRun(args, result, embedding); err != nil {
		t.Fatalf("RecordRun() error: %v", err)
	}

	// Verify the error was recorded with exit_code 1.
	results, err := sdb.SearchHistory(embedding, 1)
	if err != nil {
		t.Fatalf("SearchHistory() error: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].ExitCode != 1 {
		t.Errorf("expected exit_code 1 for error, got %d", results[0].ExitCode)
	}
}

func TestSidecarDBPath(t *testing.T) {
	dir := t.TempDir()

	skill := &Skill{
		Name:       "my_skill",
		Language:   "go",
		Dir:        dir,
		TrustLevel: TrustSecondParty,
	}

	sdb, err := OpenSidecar(skill, testEmbedDim)
	if err != nil {
		t.Fatalf("OpenSidecar() error: %v", err)
	}
	sdb.Close()

	// Verify the DB file was created at the expected path.
	expectedPath := filepath.Join(dir, "my_skill.db")
	if !fileExists(expectedPath) {
		t.Errorf("expected sidecar DB at %s, not found", expectedPath)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestFormatAge(t *testing.T) {
	now := time.Now()

	tests := []struct {
		offset time.Duration
		want   string
	}{
		{10 * time.Second, "just now"},
		{5 * time.Minute, "5m ago"},
		{2 * time.Hour, "2h ago"},
		{24 * time.Hour, "1d ago"},
		{72 * time.Hour, "3d ago"},
	}

	for _, tt := range tests {
		got := formatAge(now, now.Add(-tt.offset))
		if got != tt.want {
			t.Errorf("formatAge(-%v) = %q, want %q", tt.offset, got, tt.want)
		}
	}
}

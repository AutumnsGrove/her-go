package tools

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateReportPath(t *testing.T) {
	// Create a temp dir to act as the reports directory.
	reportsDir := t.TempDir()

	tests := []struct {
		name      string
		relPath   string
		wantErr   bool
		wantAbs   string
	}{
		{
			name:    "simple file",
			relPath: "report.md",
			wantAbs: filepath.Join(reportsDir, "report.md"),
		},
		{
			name:    "nested path",
			relPath: "daily/2026-06-10.md",
			wantAbs: filepath.Join(reportsDir, "daily/2026-06-10.md"),
		},
		{
			name:    "traversal with dot-dot",
			relPath: "../../../etc/passwd",
			wantErr: true,
		},
		{
			name:    "traversal with dot-dot-slash",
			relPath: "subdir/../../escape.txt",
			wantErr: true,
		},
		{
			name:    "absolute path injection",
			relPath: "/etc/passwd",
			wantErr: true,
		},
		{
			name:    "empty path",
			relPath: "",
			wantAbs: reportsDir,
		},
		{
			name:    "dot path",
			relPath: ".",
			wantAbs: reportsDir,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ValidateReportPath(reportsDir, tt.relPath)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error for %q, got path %q", tt.relPath, got)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error for %q: %v", tt.relPath, err)
				return
			}
			if got != tt.wantAbs {
				t.Errorf("path mismatch: got %q, want %q", got, tt.wantAbs)
			}
		})
	}
}

func TestValidateReportPath_EmptyReportsDir(t *testing.T) {
	_, err := ValidateReportPath("", "anything.md")
	if err == nil {
		t.Error("expected error when reportsDir is empty")
	}
}

func TestValidateReportPath_RealWriteRead(t *testing.T) {
	reportsDir := t.TempDir()

	absPath, err := ValidateReportPath(reportsDir, "test-report.md")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Write through the validated path.
	if err := os.WriteFile(absPath, []byte("# Test"), 0644); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	// Read it back.
	content, err := os.ReadFile(absPath)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if string(content) != "# Test" {
		t.Errorf("content mismatch: got %q", string(content))
	}
}

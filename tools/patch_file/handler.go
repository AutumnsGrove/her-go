// Package patch_file implements the patch_file tool — applies a targeted
// find-and-replace edit to a file in the reports directory. Preferred over
// write_file for small changes because it saves tokens (the agent doesn't
// need to rewrite the entire file).
package patch_file

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/patch_file")

func init() {
	tools.Register("patch_file", Handle)
}

// Handle applies a find-and-replace edit to a file in the reports directory.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		Path    string `json:"path"`
		OldText string `json:"old_text"`
		NewText string `json:"new_text"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "error: " + err.Error()
	}
	if args.Path == "" {
		return "error: path is required"
	}
	if args.OldText == "" {
		return "error: old_text is required"
	}

	absPath, err := tools.ValidateReportPath(ctx.ReportsDir, args.Path)
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}

	content, err := os.ReadFile(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Sprintf("error: file not found: %s", args.Path)
		}
		return fmt.Sprintf("error reading file: %v", err)
	}

	original := string(content)
	count := strings.Count(original, args.OldText)
	if count == 0 {
		return "error: old_text not found in file"
	}
	if count > 1 {
		return fmt.Sprintf("error: old_text matches %d locations — must be unique", count)
	}

	patched := strings.Replace(original, args.OldText, args.NewText, 1)
	if err := os.WriteFile(absPath, []byte(patched), 0644); err != nil {
		return fmt.Sprintf("error writing file: %v", err)
	}

	log.Infof("  patch_file: %s", args.Path)
	return fmt.Sprintf("patched %s (replaced %d bytes with %d bytes)",
		args.Path, len(args.OldText), len(args.NewText))
}

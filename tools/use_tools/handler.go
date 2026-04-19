// Package use_tools implements the use_tools meta-tool — the mechanism
// by which the agent loads deferred tools on demand. Instead of seeing
// all 26 tools at once, the agent starts with ~9 hot tools and calls
// use_tools(["memory", "scheduling"]) to load extras when needed.
//
// This is the Go equivalent of Claude Code's ToolSearch: reduce the
// default tool count so the model focuses on core actions, and let it
// pull in extras when actually needed.
package use_tools

import (
	"encoding/json"
	"fmt"
	"strings"

	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/use_tools")

func init() {
	tools.Register("use_tools", Handle)
}

// Handle loads deferred tools into the active tool set. The agent calls
// this to gain access to tools it needs — e.g., use_tools(["vision"])
// before calling view_image.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		Tools []string `json:"tools"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	if len(args.Tools) == 0 {
		return "no tools requested. Available categories: " + tools.CategoryDescription()
	}

	// Only allow loading by category name — not by individual tool name.
	// This prevents the main agent from loading memory-only tools (save_memory, etc.)
	// by bypassing the active-tool gate with use_tools(["save_memory"]).
	validCats := tools.Categories()
	var catNames []string
	for _, name := range args.Tools {
		if _, ok := validCats[name]; ok {
			catNames = append(catNames, name)
		}
	}
	if len(catNames) == 0 {
		return "no matching categories found. Available categories: " + tools.CategoryDescription()
	}

	newTools := tools.LookupToolDefs(catNames, ctx.Cfg)
	if len(newTools) == 0 {
		return "no matching tools found. Available categories: " + tools.CategoryDescription()
	}

	// Deduplicate — don't add tools already in the active set.
	existing := make(map[string]bool)
	for _, t := range *ctx.ActiveTools {
		existing[t.Function.Name] = true
	}

	var added []string
	for _, t := range newTools {
		if !existing[t.Function.Name] {
			*ctx.ActiveTools = append(*ctx.ActiveTools, t)
			added = append(added, t.Function.Name)
		}
	}

	if len(added) == 0 {
		return "all requested tools are already loaded"
	}

	log.Infof("  loaded deferred tools: %s", strings.Join(added, ", "))
	return fmt.Sprintf("loaded: %s. You can now call them.", strings.Join(added, ", "))
}

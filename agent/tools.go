// Package agent implements the orchestrator that drives every conversation turn.
package agent

// --- Tool Registry ---
// Tools are split into "hot" (always loaded) and "deferred" (loaded on demand).
// This reduces the number of tool schemas the agent model sees from 26 to ~9,
// improving tool selection accuracy — especially for smaller/free models that
// degrade when presented with too many options at once.
//
// Inspired by Claude Code's ToolSearch and Cloudflare's Code Mode:
// - Claude Code saw 49% → 74% accuracy by deferring niche tools
// - We go from ~2,490 tokens of tool schemas to ~900 for hot tools only
//
// Tool definitions live in YAML manifests (tools/<name>/tool.yaml), loaded
// at startup by the tools package. Handler functions live alongside each
// manifest in handler.go and register themselves via tools.Register().
//
// The agent calls use_tools(["memory", "scheduling"]) to load deferred tools
// on demand. Loaded tools persist for the rest of the agent loop.
//
// All tool management functions (HotToolDefs, LookupToolDefs, ExpandToolIdentity,
// UseToolsDef) live in the tools package (tools/loader.go). The agent package
// just calls them.

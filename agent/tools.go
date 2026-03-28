// Package agent implements the orchestrator that drives every conversation turn.
// It runs a tool-calling model that decides what to do: search the web, look up
// books, manage memory, and — most importantly — call the reply tool to generate
// and send the actual response to the user.
package agent

import (
	"her/config"
	"her/llm"
)

// --- Tool Registry ---
// Tools are split into "hot" (always loaded) and "deferred" (loaded on demand).
// This reduces the number of tool schemas the agent model sees from 26 to 7,
// improving tool selection accuracy — especially for smaller/free models that
// degrade when presented with too many options at once.
//
// Inspired by Claude Code's ToolSearch and Cloudflare's Code Mode:
// - Claude Code saw 49% → 74% accuracy by deferring niche tools
// - We go from ~2,490 tokens of tool schemas to ~900 for hot tools only
//
// The agent calls use_tools(["search"]) or use_tools(["web_search"]) to load
// deferred tools on demand. Loaded tools persist for the rest of the agent loop.

// hotToolNames lists tools that are always available to the agent.
// These are the tools used in nearly every conversation turn.
var hotToolNames = []string{
	"think",         // reasoning — used every turn
	"reply",         // generate response — REQUIRED every turn
	"done",          // signal completion — REQUIRED every turn
	"save_fact",     // save user facts — very frequent
	"update_fact",   // update existing facts — frequent
	"no_action",     // explicit skip — frequent
	"reply_confirm", // confirmation before destructive actions — always available
	"find_skill",    // KNN search over skill descriptions — skills discovery
	"run_skill",     // execute a skill binary — skills execution
}

// toolCategories groups deferred tools by function. The agent can load
// entire categories at once: use_tools(["search"]) loads all three search tools.
var toolCategories = map[string][]string{
	"search":     {"web_search", "web_read", "book_search"},
	"vision":     {"view_image"},
	"memory":     {"remove_fact", "save_self_fact", "update_persona", "recall_memories"},
	"scheduling": {"create_reminder", "create_schedule", "list_schedules", "update_schedule", "delete_schedule"},
	"context":    {"log_mood", "get_current_time", "set_location"},
	"expenses":   {"scan_receipt", "query_expenses", "delete_expense", "update_expense"},
	"skills":     {"search_history"},
}

// toolRegistry maps every tool name to its full definition.
// Built once at package init, used by HotToolDefs and LookupTools.
var toolRegistry map[string]llm.ToolDef

func init() {
	allTools := allToolDefs()
	toolRegistry = make(map[string]llm.ToolDef, len(allTools))
	for _, t := range allTools {
		toolRegistry[t.Function.Name] = t
	}
}

// expandToolIdentity replaces {{her}} and {{user}} placeholders in a tool's
// description and parameter descriptions with the configured identity names.
// Tool defs are built at init() time (before config exists), so placeholders
// are resolved per-request when tools are served to the LLM.
func expandToolIdentity(t llm.ToolDef, cfg *config.Config) llm.ToolDef {
	t.Function.Description = cfg.ExpandPrompt(t.Function.Description)

	// Also expand in parameter descriptions (one level deep).
	// We need type assertions because Parameters is interface{}.
	params, ok := t.Function.Parameters.(map[string]interface{})
	if !ok {
		return t
	}
	props, ok := params["properties"].(map[string]interface{})
	if !ok {
		return t
	}

	// Check if any descriptions need expansion before copying.
	needsCopy := false
	for _, val := range props {
		if prop, ok := val.(map[string]interface{}); ok {
			if desc, ok := prop["description"].(string); ok {
				if cfg.ExpandPrompt(desc) != desc {
					needsCopy = true
					break
				}
			}
		}
	}
	if !needsCopy {
		return t
	}

	// Copy the maps to avoid mutating the shared registry.
	newParams := make(map[string]interface{}, len(params))
	for k, v := range params {
		newParams[k] = v
	}
	newProps := make(map[string]interface{}, len(props))
	for k, v := range props {
		newProps[k] = v
	}
	newParams["properties"] = newProps
	t.Function.Parameters = newParams

	for key, val := range newProps {
		if prop, ok := val.(map[string]interface{}); ok {
			if desc, ok := prop["description"].(string); ok {
				expanded := cfg.ExpandPrompt(desc)
				if expanded != desc {
					newProp := make(map[string]interface{}, len(prop))
					for k, v := range prop {
						newProp[k] = v
					}
					newProp["description"] = expanded
					newProps[key] = newProp
				}
			}
		}
	}
	return t
}

// HotToolDefs returns the always-loaded tools plus the use_tools meta-tool.
// This is what gets passed to ChatCompletionWithTools on the first iteration.
// ~7 tools instead of 26 — a major reduction in context pressure.
// The cfg parameter is used to expand {{her}}/{{user}} placeholders in tool descriptions.
func HotToolDefs(cfg *config.Config) []llm.ToolDef {
	tools := make([]llm.ToolDef, 0, len(hotToolNames)+1)
	for _, name := range hotToolNames {
		if t, ok := toolRegistry[name]; ok {
			tools = append(tools, expandToolIdentity(t, cfg))
		}
	}
	// Add the use_tools meta-tool for loading deferred tools.
	tools = append(tools, useToolsDef())
	return tools
}

// LookupTools resolves a mix of tool names and category names into full
// tool definitions. Unknown names are silently skipped.
//
// Examples:
//
//	LookupTools(["search"])                → web_search, web_read, book_search
//	LookupTools(["web_search"])            → web_search
//	LookupTools(["search", "log_mood"])    → web_search, web_read, book_search, log_mood
func LookupTools(names []string, cfg *config.Config) []llm.ToolDef {
	seen := make(map[string]bool)
	var result []llm.ToolDef

	for _, name := range names {
		// Check if it's a category first.
		if members, ok := toolCategories[name]; ok {
			for _, member := range members {
				if !seen[member] {
					if t, ok := toolRegistry[member]; ok {
						result = append(result, expandToolIdentity(t, cfg))
						seen[member] = true
					}
				}
			}
			continue
		}
		// Otherwise treat it as a tool name.
		if !seen[name] {
			if t, ok := toolRegistry[name]; ok {
				result = append(result, expandToolIdentity(t, cfg))
				seen[name] = true
			}
		}
	}
	return result
}

// useToolsDef returns the meta-tool that loads deferred tools on demand.
func useToolsDef() llm.ToolDef {
	return llm.ToolDef{
		Type: "function",
		Function: llm.ToolFunctionDef{
			Name:        "use_tools",
			Description: "Load additional tools you need for this turn. Call BEFORE using a deferred tool. Pass category names or individual tool names. Loaded tools stay available for the rest of this turn.\n\nCategories: search (web_search, web_read, book_search) | vision (view_image) | memory (remove_fact, save_self_fact, update_persona, recall_memories) | scheduling (create_reminder, create_schedule, list_schedules, update_schedule, delete_schedule) | context (log_mood, get_current_time, set_location) | expenses (scan_receipt, query_expenses, delete_expense, update_expense)",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"tools": map[string]interface{}{
						"type":        "array",
						"items":       map[string]interface{}{"type": "string"},
						"description": "Tool names or category names to load. E.g., [\"search\"], [\"vision\", \"scheduling\"], [\"web_search\", \"log_mood\"]",
					},
				},
				"required": []string{"tools"},
			},
		},
	}
}

// allToolDefs returns every tool definition in the system.
// This is called once at init to populate the registry.
// If you add a new tool, add it here AND update agent_prompt.md.
func allToolDefs() []llm.ToolDef {
	return []llm.ToolDef{
		// =====================================================================
		// HOT TOOLS — always loaded, used nearly every turn
		// =====================================================================

		// --- Response (REQUIRED every turn) ---
		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "reply",
				Description: "Generate and send a response to the user. REQUIRED — you MUST call this at least once per turn. The instruction is a DIRECTIVE to a separate conversational model — describe what to say, not the words themselves.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"instruction": map[string]interface{}{
							"type":        "string",
							"description": "A directive to the conversational model describing what to respond about, the tone, and key points. Do NOT write the actual reply text — another model generates that. Example: 'Warmly greet the user and ask about their day' NOT 'hey! how's your day going?'",
						},
						"context": map[string]interface{}{
							"type":        "string",
							"description": "Optional additional context (search results, book data, URL content) to include in the prompt.",
						},
					},
					"required": []string{"instruction"},
				},
			},
		},

		// --- Reasoning ---
		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "think",
				Description: "Pause and reason before acting. Use this to evaluate search results, resolve contradictions in memory, plan your next step, or decide between multiple approaches. This tool does nothing except give you space to think — call it whenever you need to deliberate before making a decision.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"thought": map[string]interface{}{
							"type":        "string",
							"description": "Your internal reasoning. What are you considering? What's the best next step?",
						},
					},
					"required": []string{"thought"},
				},
			},
		},

		// --- Memory (hot) ---
		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "save_fact",
				Description: "Save a new fact learned from the conversation. Use for new information about {{user}} that's worth remembering long-term.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"fact": map[string]interface{}{
							"type":        "string",
							"description": "A clear, single-sentence fact about {{user}}",
						},
						"category": map[string]interface{}{
							"type":        "string",
							"enum":        []string{"identity", "relationship", "health", "work", "mood", "goal", "event", "preference", "context", "other"},
							"description": "Category of the fact. Use 'context' for day-to-day activities that change frequently (e.g., what the user is working on today). Context facts get auto-timestamped and should be updated rather than duplicated.",
						},
						"importance": map[string]interface{}{
							"type":        "integer",
							"minimum":     1,
							"maximum":     10,
							"description": "How important this is to remember (1=trivial, 10=life-changing)",
						},
						"tags": map[string]interface{}{
							"type":        "string",
							"description": "Comma-separated topic tags for semantic search — describes WHAT this fact is about so it surfaces in the right conversations. Example: 'mental health, burnout, coping, energy' or 'programming, go, backend, projects'. Be specific to the fact's topic.",
						},
					},
					"required": []string{"fact", "category", "importance", "tags"},
				},
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "update_fact",
				Description: "Update an existing fact when new information changes or refines what we knew. Use this instead of save_fact when a fact already exists but needs correction or expansion.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"fact_id": map[string]interface{}{
							"type":        "integer",
							"description": "ID of the fact to update",
						},
						"fact": map[string]interface{}{
							"type":        "string",
							"description": "The updated fact text",
						},
						"category": map[string]interface{}{
							"type":        "string",
							"enum":        []string{"identity", "relationship", "health", "work", "mood", "goal", "event", "preference", "context", "other"},
							"description": "Updated category. Use 'context' for ephemeral day-to-day info.",
						},
						"importance": map[string]interface{}{
							"type":        "integer",
							"minimum":     1,
							"maximum":     10,
							"description": "Updated importance score",
						},
						"tags": map[string]interface{}{
							"type":        "string",
							"description": "Updated comma-separated topic tags for semantic search.",
						},
					},
					"required": []string{"fact_id", "fact", "category", "importance", "tags"},
				},
			},
		},

		// --- Control ---
		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "no_action",
				Description: "Explicitly skip memory management. Use when the exchange doesn't reveal new information worth saving. You still MUST call reply and done.",
				Parameters: map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "done",
				Description: "Signal that you are completely finished with this turn. Call this LAST, after reply and any memory operations. Every turn MUST end with done.",
				Parameters: map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
		},

		// --- Confirmation ---
		//
		// reply_confirm sends a Yes/No inline keyboard for destructive actions.
		// Instead of the agent executing delete_expense directly, it calls
		// reply_confirm with the action details. The user clicks a button,
		// and the callback handler executes (or cancels) the action.
		// This prevents the "lost context" problem where a plain-text "are
		// you sure?" causes the user's "yes" to arrive as a new turn.
		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "reply_confirm",
				Description: "Send a Yes/No confirmation prompt with inline buttons before executing a destructive action. The action executes ONLY when the user clicks Yes. Use this for: deleting expenses, removing facts, deleting schedules. After calling this, proceed to reply and done — the action runs asynchronously when the user responds.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"message": map[string]interface{}{
							"type":        "string",
							"description": "The confirmation question to display. Be specific about what will happen. Example: 'Delete the £47.23 Trader Joe's expense from March 25?'",
						},
						"action_type": map[string]interface{}{
							"type":        "string",
							"enum":        []string{"delete_expense", "remove_fact", "delete_schedule"},
							"description": "The type of destructive action to execute on confirmation.",
						},
						"action_payload": map[string]interface{}{
							"type":        "string",
							"description": "JSON payload for the action. delete_expense: {\"id\": 42} for one, or {\"ids\": [1,2,3]} for multiple. remove_fact: {\"fact_id\": 17}. delete_schedule: {\"task_id\": 5}.",
						},
					},
					"required": []string{"message", "action_type", "action_payload"},
				},
			},
		},

		// --- Skills (hot) ---
		//
		// find_skill and run_skill are always available. The agent uses
		// find_skill to discover skills by intent (KNN semantic search),
		// then run_skill to execute the matched skill in a sandbox.
		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "find_skill",
				Description: "Search for a skill by describing what you need. Returns matching skills ranked by relevance. Use this when you need a capability that isn't one of your built-in tools.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"query": map[string]interface{}{
							"type":        "string",
							"description": "Natural language description of what you need (e.g., 'get weather forecast', 'search the web', 'look up a book').",
						},
					},
					"required": []string{"query"},
				},
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "run_skill",
				Description: "Execute a skill by name with the given arguments. Use find_skill first to discover available skills, then call this with the skill name and a JSON object of arguments matching the skill's parameter schema.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"name": map[string]interface{}{
							"type":        "string",
							"description": "Exact skill name (from find_skill results).",
						},
						"args": map[string]interface{}{
							"type":        "object",
							"description": "Arguments to pass to the skill, matching its parameter schema.",
						},
					},
					"required": []string{"name"},
				},
			},
		},

		// =====================================================================
		// DEFERRED TOOLS — loaded on demand via use_tools
		// =====================================================================

		// --- Skills (category: "skills") ---
		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "search_history",
				Description: "Search a skill's execution history for cached results. Returns past runs ranked by relevance, with freshness metadata. Use before re-running a skill to check if you already have a recent answer.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"skill_name": map[string]interface{}{
							"type":        "string",
							"description": "Exact skill name to search history for.",
						},
						"query": map[string]interface{}{
							"type":        "string",
							"description": "What to search for in past results (natural language).",
						},
					},
					"required": []string{"skill_name", "query"},
				},
			},
		},

		// --- Search (category: "search") ---
		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "web_search",
				Description: "Search the web for current information. Use when the user asks about something you don't know, current events, factual questions, or anything that benefits from real-time data.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"query": map[string]interface{}{
							"type":        "string",
							"description": "A concise, specific search query",
						},
					},
					"required": []string{"query"},
				},
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "web_read",
				Description: "Read a specific URL to get its content. Use when the user shares a link or you need details from a specific web page.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"url": map[string]interface{}{
							"type":        "string",
							"description": "The URL to read and extract content from",
						},
					},
					"required": []string{"url"},
				},
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "book_search",
				Description: "Search for book information using Open Library. Use when discussing books, looking for recommendations, or when the user mentions a book title or author.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"query": map[string]interface{}{
							"type":        "string",
							"description": "Book title, author, or search terms",
						},
					},
					"required": []string{"query"},
				},
			},
		},

		// --- Vision (category: "vision") ---
		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "view_image",
				Description: "Analyze an image the user sent. Returns a detailed description of what's in it. Call this BEFORE reply when the user sends a photo.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"prompt": map[string]interface{}{
							"type":        "string",
							"description": "What to focus on when describing the image. E.g., 'describe this photo', 'what food is this', 'read any text in this image'.",
						},
					},
					"required": []string{"prompt"},
				},
			},
		},

		// --- Memory extras (category: "memory") ---
		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "remove_fact",
				Description: "Remove a fact that is no longer true, was incorrect, or is redundant with another fact. The fact is soft-deleted (kept for audit but excluded from memory).",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"fact_id": map[string]interface{}{
							"type":        "integer",
							"description": "ID of the fact to remove",
						},
						"reason": map[string]interface{}{
							"type":        "string",
							"description": "Brief reason for removal (for logging)",
						},
					},
					"required": []string{"fact_id", "reason"},
				},
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "save_self_fact",
				Description: "Save a fact about {{her}} herself — her own observations, communication patterns, identity, or things she's learned about the relationship dynamic. NOT for facts about {{user}}.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"fact": map[string]interface{}{
							"type":        "string",
							"description": "A self-observation or identity fact about {{her}}",
						},
						"category": map[string]interface{}{
							"type":        "string",
							"enum":        []string{"identity", "observation", "pattern", "preference", "relationship_dynamic"},
							"description": "Category of the self-fact",
						},
						"importance": map[string]interface{}{
							"type":        "integer",
							"minimum":     1,
							"maximum":     10,
							"description": "How important this is (1=minor note, 10=core identity)",
						},
						"tags": map[string]interface{}{
							"type":        "string",
							"description": "Comma-separated topic tags for semantic search — describes WHAT this self-fact is about. Example: 'identity, communication style, authenticity' or 'relationship, boundaries, user dynamic'.",
						},
					},
					"required": []string{"fact", "category", "importance", "tags"},
				},
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "update_persona",
				Description: "Rewrite {{her}}'s persona description. Use RARELY — only when accumulated self-facts suggest a meaningful evolution in how {{her}} sees herself. The persona should evolve gradually.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"content": map[string]interface{}{
							"type":        "string",
							"description": "The full new persona.md content. Should preserve core personality while incorporating growth.",
						},
						"reason": map[string]interface{}{
							"type":        "string",
							"description": "Brief explanation of what changed and why",
						},
					},
					"required": []string{"content", "reason"},
				},
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "recall_memories",
				Description: "Search through stored memories using semantic similarity. Use when the user asks 'do you remember...', references something from a past conversation, or when you need specific context that isn't in the current memory window.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"query": map[string]interface{}{
							"type":        "string",
							"description": "What to search for in memory. Be specific — 'user's dog' works better than 'pet'.",
						},
						"limit": map[string]interface{}{
							"type":        "integer",
							"description": "Max results to return (default 5, max 10)",
						},
					},
					"required": []string{"query"},
				},
			},
		},

		// --- Scheduling (category: "scheduling") ---
		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "create_reminder",
				Description: "Create a one-shot reminder that fires at a specific time. Convert natural language times to ISO 8601 timestamps.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"message": map[string]interface{}{
							"type":        "string",
							"description": "What to remind the user about",
						},
						"trigger_at": map[string]interface{}{
							"type":        "string",
							"description": "When to fire, as ISO 8601 datetime (e.g., '2026-03-24T15:00:00')",
						},
						"natural_time": map[string]interface{}{
							"type":        "string",
							"description": "The original natural language time (e.g., 'tomorrow at 3pm')",
						},
					},
					"required": []string{"message", "trigger_at"},
				},
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "create_schedule",
				Description: "Create a recurring scheduled task with a cron expression.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"name": map[string]interface{}{
							"type":        "string",
							"description": "Human-readable name (e.g., 'morning briefing')",
						},
						"cron_expr": map[string]interface{}{
							"type":        "string",
							"description": "Cron expression: '0 8 * * *' (8am daily), '0 9 * * 1-5' (9am weekdays)",
						},
						"task_type": map[string]interface{}{
							"type":        "string",
							"enum":        []string{"run_prompt", "send_message"},
							"description": "'run_prompt' runs through the full agent pipeline. 'send_message' sends a static message.",
						},
						"payload": map[string]interface{}{
							"type":        "object",
							"description": "Task config — for run_prompt: {\"prompt\": \"...\"}, for send_message: {\"message\": \"...\"}",
						},
						"priority": map[string]interface{}{
							"type":        "string",
							"enum":        []string{"normal", "high", "critical"},
							"description": "Priority level. Default: 'normal'.",
						},
						"max_runs": map[string]interface{}{
							"type":        "integer",
							"description": "Maximum number of executions. Omit for unlimited.",
						},
						"description": map[string]interface{}{
							"type":        "string",
							"description": "What this schedule does, in plain English",
						},
					},
					"required": []string{"name", "cron_expr", "task_type", "payload"},
				},
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "list_schedules",
				Description: "List all active scheduled tasks with next run times.",
				Parameters: map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "update_schedule",
				Description: "Pause or resume an existing scheduled task by ID.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"task_id": map[string]interface{}{
							"type":        "integer",
							"description": "ID of the scheduled task to update",
						},
						"enabled": map[string]interface{}{
							"type":        "boolean",
							"description": "true to enable/resume, false to pause",
						},
					},
					"required": []string{"task_id", "enabled"},
				},
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "delete_schedule",
				Description: "Permanently delete a scheduled task by ID.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"task_id": map[string]interface{}{
							"type":        "integer",
							"description": "ID of the scheduled task to delete",
						},
					},
					"required": []string{"task_id"},
				},
			},
		},

		// --- Context (category: "context") ---
		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "log_mood",
				Description: "Log the user's current emotional state when they express how they're feeling.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"rating": map[string]interface{}{
							"type":        "integer",
							"minimum":     1,
							"maximum":     5,
							"description": "Mood rating: 1=bad, 2=rough, 3=meh/neutral, 4=good, 5=great",
						},
						"note": map[string]interface{}{
							"type":        "string",
							"description": "Brief context for the rating",
						},
					},
					"required": []string{"rating", "note"},
				},
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "set_location",
				Description: "Set the user's location by city/place name. Enables weather data in conversations. Coordinates are saved to config automatically — do NOT save a separate location fact for coordinates. The user's natural-language location (city, state) is already captured in regular conversation facts.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"query": map[string]interface{}{
							"type":        "string",
							"description": "City and state/country name (e.g., 'Portland Oregon', 'Tokyo')",
						},
					},
					"required": []string{"query"},
				},
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "get_current_time",
				Description: "Get the current date and time in the user's timezone.",
				Parameters: map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
		},

		// --- Expense Tracking ---

		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name: "scan_receipt",
				Description: "Save a purchase/expense from a receipt photo or manual mention. " +
					"Use when OCR text from a photo looks like a receipt (amounts, totals, store names), " +
					"or when {{user}} mentions spending money in conversation. " +
					"Extract the total amount, vendor name, category, and date from the OCR text. " +
					"If the date isn't visible on the receipt, use today's date. " +
					"IMPORTANT: Extract ALL line items from the receipt, including discounts (as negative amounts), " +
					"overrides, and adjustments. Every line with a price should be an item.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"amount": map[string]interface{}{
							"type":        "number",
							"description": "Total amount spent (e.g., 47.23). Just the number, no currency symbol.",
						},
						"vendor": map[string]interface{}{
							"type":        "string",
							"description": "Store or restaurant name as shown on the receipt (e.g., 'Trader Joe's', 'Shell Gas Station').",
						},
						"category": map[string]interface{}{
							"type": "string",
							"enum": []string{
								"groceries", "dining", "coffee", "transport", "shopping",
								"entertainment", "health", "utilities", "housing",
								"subscriptions", "personal_care", "other",
							},
							"description": "Expense category. Pick the closest match from the list.",
						},
						"date": map[string]interface{}{
							"type":        "string",
							"description": "Purchase date in YYYY-MM-DD format. Use the date printed on the receipt if visible, otherwise use today's date.",
						},
						"currency": map[string]interface{}{
							"type":        "string",
							"description": "ISO 4217 currency code (default: USD). Only set if the receipt clearly shows a different currency.",
						},
						"note": map[string]interface{}{
							"type":        "string",
							"description": "Optional brief note about the purchase (e.g., 'weekly grocery run', 'birthday dinner').",
						},
						"items": map[string]interface{}{
							"type":        "array",
							"description": "ALL individual line items from the receipt. Extract EVERY line that has a price — products, discounts, overrides, adjustments. Use negative total_price for discounts/refunds (e.g., -2.00 for a £2 discount).",
							"items": map[string]interface{}{
								"type": "object",
								"properties": map[string]interface{}{
									"description": map[string]interface{}{
										"type":        "string",
										"description": "Item name as shown on receipt (e.g., 'Bulmers Original Bottle').",
									},
									"quantity": map[string]interface{}{
										"type":        "integer",
										"description": "Number of this item (default 1).",
									},
									"unit_price": map[string]interface{}{
										"type":        "number",
										"description": "Price per unit.",
									},
									"total_price": map[string]interface{}{
										"type":        "number",
										"description": "Total price for this line (quantity * unit_price, or the line total shown on receipt).",
									},
								},
								"required": []string{"description", "total_price"},
							},
						},
					},
					"required": []string{"amount", "vendor", "category", "date"},
				},
			},
		},

		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name: "query_expenses",
				Description: "Look up {{user}}'s expense history. Use when they ask about spending, finances, " +
					"budgets, or how much they've spent. Returns totals, category breakdowns, and recent transactions.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"period": map[string]interface{}{
							"type":        "string",
							"enum":        []string{"week", "month", "year", "all"},
							"description": "Time period to query. 'week' = this week, 'month' = this month, 'year' = this year, 'all' = everything.",
						},
						"category": map[string]interface{}{
							"type":        "string",
							"description": "Optional category filter (e.g., 'groceries', 'dining'). Omit to see all categories.",
						},
					},
					"required": []string{"period"},
				},
			},
		},

		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "delete_expense",
				Description: "Delete an expense by ID. Use when {{user}} wants to remove a mistaken or test entry. Also deletes all associated line items.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"id": map[string]interface{}{
							"type":        "integer",
							"description": "The expense ID to delete (shown in query_expenses results).",
						},
					},
					"required": []string{"id"},
				},
			},
		},

		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "update_expense",
				Description: "Update fields on an existing expense. Use when {{user}} wants to correct a scanned receipt (wrong amount, vendor, category, or date). Only pass the fields that need changing.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"id": map[string]interface{}{
							"type":        "integer",
							"description": "The expense ID to update.",
						},
						"amount": map[string]interface{}{
							"type":        "number",
							"description": "Corrected total amount (omit to keep current).",
						},
						"vendor": map[string]interface{}{
							"type":        "string",
							"description": "Corrected vendor name (omit to keep current).",
						},
						"category": map[string]interface{}{
							"type": "string",
							"enum": []string{
								"groceries", "dining", "coffee", "transport", "shopping",
								"entertainment", "health", "utilities", "housing",
								"subscriptions", "personal_care", "other",
							},
							"description": "Corrected category (omit to keep current).",
						},
						"date": map[string]interface{}{
							"type":        "string",
							"description": "Corrected date in YYYY-MM-DD format (omit to keep current).",
						},
						"currency": map[string]interface{}{
							"type":        "string",
							"description": "Corrected currency code (omit to keep current).",
						},
						"note": map[string]interface{}{
							"type":        "string",
							"description": "Updated note (omit to keep current).",
						},
					},
					"required": []string{"id"},
				},
			},
		},
	}
}

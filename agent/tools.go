// Package agent implements the orchestrator that drives every conversation turn.
// It runs a tool-calling model that decides what to do: search the web, look up
// books, manage memory, and — most importantly — call the reply tool to generate
// and send the actual response to the user.
package agent

import "her/llm"

// ToolDefs returns the tool definitions available to the agent.
// These follow the OpenAI function calling format — each tool has a name,
// description, and a JSON Schema describing its parameters.
//
// Think of these like Python function signatures with type hints — they
// tell the model what it can call and what arguments each function expects.
func ToolDefs() []llm.ToolDef {
	return []llm.ToolDef{
		// --- Response tool (REQUIRED every turn) ---
		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "reply",
				Description: "Generate and send a response to the user. REQUIRED — you MUST call this exactly once per turn. Call it after gathering any context you need (search results, book info, etc.). The instruction tells the conversational model what to respond about.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"instruction": map[string]interface{}{
							"type":        "string",
							"description": "What to tell the conversational model to respond about. Include any search results or context. Example: 'User asked about the weather in Portland. Respond warmly and mention it looks rainy.'",
						},
						"context": map[string]interface{}{
							"type":        "string",
							"description": "Optional additional context (search results, book data, URL content) to include in the prompt. If you searched or read something, paste the relevant results here.",
						},
					},
					"required": []string{"instruction"},
				},
			},
		},

		// --- Search tools ---
		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "web_search",
				Description: "Search the web for current information. Use when the user asks about something you don't know, current events, factual questions, or anything that benefits from real-time data. Search BEFORE calling reply.",
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

		// --- Vision tool ---
		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "view_image",
				Description: "Analyze an image the user sent. Returns a detailed description of what's in it. Call this BEFORE reply when the user sends a photo, so you can talk about the image in your response.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"prompt": map[string]interface{}{
							"type":        "string",
							"description": "What to focus on when describing the image. E.g., 'describe this photo', 'what food is this', 'read any text in this image'. Tailor this to what the user seems interested in.",
						},
					},
					"required": []string{"prompt"},
				},
			},
		},

		// --- Memory management tools ---
		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "save_fact",
				Description: "Save a new fact learned from the conversation. Use for new information about the user that's worth remembering long-term.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"fact": map[string]interface{}{
							"type":        "string",
							"description": "A clear, single-sentence fact about the user",
						},
						"category": map[string]interface{}{
							"type":        "string",
							"enum":        []string{"identity", "relationship", "health", "work", "mood", "goal", "event", "preference", "other"},
							"description": "Category of the fact",
						},
						"importance": map[string]interface{}{
							"type":        "integer",
							"minimum":     1,
							"maximum":     10,
							"description": "How important this is to remember (1=trivial, 10=life-changing)",
						},
					},
					"required": []string{"fact", "category", "importance"},
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
							"enum":        []string{"identity", "relationship", "health", "work", "mood", "goal", "event", "preference", "other"},
							"description": "Updated category",
						},
						"importance": map[string]interface{}{
							"type":        "integer",
							"minimum":     1,
							"maximum":     10,
							"description": "Updated importance score",
						},
					},
					"required": []string{"fact_id", "fact", "category", "importance"},
				},
			},
		},
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
				Description: "Save a fact about Mira herself — her own observations, communication patterns, identity, or things she's learned about the relationship dynamic. NOT for facts about the user.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"fact": map[string]interface{}{
							"type":        "string",
							"description": "A self-observation or identity fact about Mira",
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
					},
					"required": []string{"fact", "category", "importance"},
				},
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "update_persona",
				Description: "Rewrite Mira's persona description. Use RARELY — only when accumulated self-facts suggest a meaningful evolution in how Mira sees herself. The persona should evolve gradually.",
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
		// --- Scheduler tools ---
		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "create_reminder",
				Description: "Create a one-shot reminder that fires at a specific time. Use when the user asks to be reminded of something. You MUST convert natural language times to an absolute ISO 8601 timestamp (e.g., 'tomorrow at 3pm' → '2026-03-23T15:00:00'). Always confirm what you're setting with the user in your reply.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"message": map[string]interface{}{
							"type":        "string",
							"description": "What to remind the user about",
						},
						"trigger_at": map[string]interface{}{
							"type":        "string",
							"description": "When to fire the reminder, as ISO 8601 datetime (e.g., '2026-03-22T15:00:00'). Convert the user's natural language time to this format.",
						},
						"natural_time": map[string]interface{}{
							"type":        "string",
							"description": "The original natural language time from the user (e.g., 'tomorrow at 3pm'). Used for the confirmation message.",
						},
					},
					"required": []string{"message", "trigger_at"},
				},
			},
		},

		// --- Schedule management tools (v0.6) ---

		// create_schedule creates recurring or conditional scheduled tasks.
		// Unlike create_reminder (one-shot), this is for things that repeat.
		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "create_schedule",
				Description: "Create a recurring scheduled task. Use for things like daily check-ins, morning briefings, or periodic follow-ups. Requires a cron expression. Common patterns: '0 8 * * *' (8am daily), '0 21 * * *' (9pm daily), '0 9 * * 1-5' (9am weekdays), '@every 30m' (every 30 minutes).",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"name": map[string]interface{}{
							"type":        "string",
							"description": "Human-readable name (e.g., 'morning briefing', 'mood check-in')",
						},
						"cron_expr": map[string]interface{}{
							"type":        "string",
							"description": "Cron expression: minute hour day-of-month month day-of-week. Examples: '0 8 * * *' (8am daily), '30 9 * * 1-5' (9:30am weekdays), '@every 2h' (every 2 hours)",
						},
						"task_type": map[string]interface{}{
							"type":        "string",
							"enum":        []string{"run_prompt", "send_message"},
							"description": "Type of task. 'run_prompt' runs through the full agent pipeline (can use tools, memory, etc). 'send_message' sends a static message.",
						},
						"payload": map[string]interface{}{
							"type":        "object",
							"description": "Task config — for run_prompt: {\"prompt\": \"...\"}, for send_message: {\"message\": \"...\"}",
						},
						"priority": map[string]interface{}{
							"type":        "string",
							"enum":        []string{"normal", "high", "critical"},
							"description": "Priority level. 'normal' = subject to all damping, 'high' = bypasses rate limits, 'critical' = always fires. Default: 'normal'.",
						},
						"max_runs": map[string]interface{}{
							"type":        "integer",
							"description": "Maximum number of executions. Omit for unlimited (runs forever until paused/deleted).",
						},
						"description": map[string]interface{}{
							"type":        "string",
							"description": "What this schedule does, in plain English (for the confirmation message)",
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
				Description: "List all active scheduled tasks. Shows recurring jobs, upcoming reminders, and their next run times. Use when the user asks what's scheduled or wants to see their recurring tasks.",
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
				Description: "Pause or resume an existing scheduled task by ID. Use when the user wants to temporarily disable or re-enable a recurring task.",
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
				Description: "Permanently delete a scheduled task by ID. Use when the user wants to remove a recurring task entirely (not just pause it).",
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

		// --- Memory search tool ---
		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "recall_memories",
				Description: "Search through stored memories using semantic similarity. Use when the user asks 'do you remember...', references something from a past conversation, or when you need specific context that isn't in the current memory window. Returns the most relevant facts matching your query.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"query": map[string]interface{}{
							"type":        "string",
							"description": "What to search for in memory. Be specific — 'user's dog' works better than 'pet'. Rephrase the user's question into a factual search query.",
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

		// --- Mood tracking tool ---
		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "log_mood",
				Description: "Log the user's current emotional state when they express how they're feeling. Use this when the user says things like 'I'm having a rough day', 'feeling great', 'stressed out', etc. Don't log mood for purely informational messages.",
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
							"description": "Brief context for the rating (e.g., 'stressed about work deadline', 'excited about weekend plans')",
						},
					},
					"required": []string{"rating", "note"},
				},
			},
		},

		// --- Reasoning tool ---
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
		// --- Time tool ---
		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "get_current_time",
				Description: "Get the current date and time in the user's timezone. Use when you need to know what time it is, what day of the week it is, or to reason about timing (e.g., 'is it morning or evening?', 'is this reminder for today or tomorrow?').",
				Parameters: map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
		},
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
		// --- Control tool ---
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
	}
}

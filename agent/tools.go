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

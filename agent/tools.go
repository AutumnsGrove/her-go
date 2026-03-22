// Package agent implements the background tool-calling layer that manages
// Mira's memory. It runs a lightweight model (Liquid LFM) that decides
// what to do with each conversation exchange: save facts, update existing
// ones, remove outdated info, or trigger a follow-up message.
package agent

import "her-go/llm"

// ToolDefs returns the tool definitions available to the agent.
// These follow the OpenAI function calling format — each tool has a name,
// description, and a JSON Schema describing its parameters.
//
// Think of these like Python function signatures with type hints — they
// tell the model what it can call and what arguments each function expects.
func ToolDefs() []llm.ToolDef {
	return []llm.ToolDef{
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
		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "send_message",
				Description: "Send a follow-up message to the user. Only use this when the user asked Mira to DO something (look something up, recall a memory, etc.) and the result is ready. Do NOT use for casual conversation — Mira already responded.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"instruction": map[string]interface{}{
							"type":        "string",
							"description": "What Mira should say or do in the follow-up message. This gets sent to the conversational model to generate a natural response.",
						},
					},
					"required": []string{"instruction"},
				},
			},
		},
		{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        "no_action",
				Description: "Explicitly do nothing. Use this when the conversation exchange doesn't contain any new facts worth saving, no facts need updating, and no follow-up is needed.",
				Parameters: map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
		},
	}
}

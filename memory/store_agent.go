package memory

import (
	"fmt"
)

// AgentAction represents a single tool call from a previous turn,
// loaded from agent_turns for the agent's action history context.
// Paired: the assistant row (tool call) and tool row (result) are
// combined into one struct so the agent sees call + outcome together.
type AgentAction struct {
	MessageID int64  // which user message triggered this action
	ToolName  string // e.g. "save_memory", "web_search", "recall_memories"
	ToolArgs  string // JSON arguments (may be truncated for verbose tools)
	Result    string // tool result (may be truncated for verbose tools)
}

// RecentAgentActions loads the agent's tool call history for recent messages
// in a specific conversation. It pairs each "assistant" row (the tool call)
// with its following "tool" row (the result) into AgentAction structs.
// Returns actions oldest-first within the most recent N message IDs.
//
// The limit controls how many message IDs worth of actions to load (not
// individual tool calls). A message that triggered 5 tool calls returns
// all 5 as separate AgentAction structs.
func (s *SQLiteStore) RecentAgentActions(conversationID string, messageLimit int) ([]AgentAction, error) {
	if messageLimit <= 0 {
		messageLimit = 20
	}

	// Get the most recent message IDs that have agent turns.
	// We use a subquery to get the last N distinct message_ids,
	// then load all turns for those messages. The JOIN on messages
	// ensures we only pull actions from the requested conversation.
	rows, err := s.db.Query(
		`SELECT at.message_id, at.turn_index, at.role, at.tool_name, at.tool_args, at.content
		 FROM agent_turns at
		 JOIN messages m ON at.message_id = m.id
		 WHERE m.conversation_id = ? AND at.message_id IN (
			 SELECT DISTINCT at2.message_id FROM agent_turns at2
			 JOIN messages m2 ON at2.message_id = m2.id
			 WHERE at2.message_id IS NOT NULL AND m2.conversation_id = ?
			 ORDER BY at2.message_id DESC
			 LIMIT ?
		 )
		 ORDER BY at.message_id ASC, at.turn_index ASC`,
		conversationID, conversationID, messageLimit,
	)
	if err != nil {
		return nil, fmt.Errorf("querying agent actions: %w", err)
	}
	defer rows.Close()

	// Pair assistant (call) + tool (result) rows into AgentAction structs.
	// The agent_turns table alternates: assistant row, then tool row.
	var actions []AgentAction
	var pending *AgentAction // waiting for its tool result

	for rows.Next() {
		var msgID int64
		var turnIndex int
		var role, toolName, toolArgs, content string
		if err := rows.Scan(&msgID, &turnIndex, &role, &toolName, &toolArgs, &content); err != nil {
			return nil, fmt.Errorf("scanning agent turn: %w", err)
		}

		if role == "assistant" {
			// This is a tool call — start a new pending action.
			if pending != nil {
				// Previous call had no result (shouldn't happen, but be safe).
				actions = append(actions, *pending)
			}
			pending = &AgentAction{
				MessageID: msgID,
				ToolName:  toolName,
				ToolArgs:  toolArgs,
			}
		} else if role == "tool" && pending != nil {
			// This is the result for the pending call.
			pending.Result = content
			actions = append(actions, *pending)
			pending = nil
		}
	}
	if pending != nil {
		actions = append(actions, *pending)
	}
	return actions, rows.Err()
}

// SaveAgentTurn logs a single step in the agent's reasoning chain.
// turnIndex is the sequential position within the agent run (0, 1, 2...).
// role is "assistant" (agent's decision) or "tool" (tool result).
func (s *SQLiteStore) SaveAgentTurn(messageID int64, turnIndex int, role, toolName, toolArgs, content string) error {
	var msgID interface{} = messageID
	if messageID == 0 {
		msgID = nil
	}
	_, err := s.db.Exec(
		`INSERT INTO agent_turns (message_id, turn_index, role, tool_name, tool_args, content)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		msgID, turnIndex, role, toolName, toolArgs, content,
	)
	if err != nil {
		return fmt.Errorf("saving agent turn: %w", err)
	}
	return nil
}

// SaveSearch logs a search operation (web, book, or URL read) for
// full observability. Tracks what was searched, what came back, and
// which user message triggered it.
func (s *SQLiteStore) SaveSearch(messageID int64, searchType, query, results string, resultCount int) error {
	var msgID interface{} = messageID
	if messageID == 0 {
		msgID = nil
	}
	_, err := s.db.Exec(
		`INSERT INTO searches (message_id, search_type, query, results, result_count)
		 VALUES (?, ?, ?, ?, ?)`,
		msgID, searchType, query, results, resultCount,
	)
	if err != nil {
		return fmt.Errorf("saving search: %w", err)
	}
	return nil
}

// SaveClassifierLog records a classifier decision (both SAVE and rejections).
// Every call to the classifier gate is logged here so we can track false
// positive rates, tune prompts, and debug rejection patterns without
// scraping her.log.
func (s *SQLiteStore) SaveClassifierLog(conversationID, writeType, verdict, content, reason, rewrite string) error {
	var rewriteVal interface{}
	if rewrite != "" {
		rewriteVal = rewrite
	}
	_, err := s.db.Exec(
		`INSERT INTO classifier_log (conversation_id, write_type, verdict, content, reason, rewrite)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		conversationID, writeType, verdict, content, reason, rewriteVal,
	)
	if err != nil {
		return fmt.Errorf("saving classifier log: %w", err)
	}
	return nil
}

// LogCommand records a slash command the user ran. This goes into the
// command_log table for usage analytics — how often /clear is used, etc.
func (s *SQLiteStore) LogCommand(command string, chatID int64, conversationID, args string) {
	_, err := s.db.Exec(
		`INSERT INTO command_log (command, chat_id, conversation_id, args)
		 VALUES (?, ?, ?, ?)`,
		command, chatID, conversationID, args,
	)
	if err != nil {
		log.Error("saving command log", "err", err)
	}
}

package layers

// Agent layer: Self-memory is retrieved on demand via recall_memories,
// not auto-injected. The memory recall prompt in agent_user_facts.go
// already covers this — self facts appear in recall_memories results
// alongside user facts (filtered by subject in the tool output).
//
// This file is intentionally empty — the layer is not registered.
// Keeping the file avoids a confusing gap in the layer numbering.

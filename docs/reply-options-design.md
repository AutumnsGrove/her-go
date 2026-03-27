# reply_options Tool — Design Document

Saved 2026-03-27. Not yet implemented — parked for later.

## What it does

General-purpose tool that lets the agent present 2-4 options as Telegram inline keyboard buttons. When the user clicks one, the selection feeds back into the agent as a new turn (the agent decides what to do next). No stored actions — purely conversational.

## Use cases

- **Disambiguation**: "Did you mean the Portland trip or the Portland apartment?"
- **Guided choices**: "Which expense to delete?" with specific options listed
- Any "pick one" situation where free-text would be ambiguous

## Design decisions made

- **Feed back to agent, not execute stored actions** — clicking an option triggers a new `agent.Run()` with the selection as a synthetic prompt. The agent then acts on it naturally (reply, search, save fact, etc). Most flexible approach.
- **Hot tool** (always loaded) — disambiguation can happen on any turn, same reasoning as reply_confirm.
- **Reuses `pending_confirmations` table** — stores `action_type="options"` with `action_payload={"question":"...","options":["A","B","C"]}`. Looked up by Telegram msg ID on click. No new table needed.
- **Separate callback handler** — `handleOptionCallback` registered on `&tele.InlineButton{Unique: "option"}`, distinct from the `"confirm"` handler.
- **Follows `runMoodFollowUp` pattern** — callback runs `agent.Run()` in a goroutine with a synthetic prompt like: `"User was asked: '<question>' and selected: '<option>'. Act on their choice."`
- **1-hour TTL + double-click protection** — same as reply_confirm, via the existing `GetPendingConfirmation` query.

## Tool definition

```
reply_options — present multiple-choice options as inline buttons
Parameters:
  - question (string, required): what you're asking
  - options (array of strings, required, 2-4 items): the choices
```

## New callback type needed

```go
type SendOptionsCallback func(question string, options []string) (telegramMsgID int64, err error)
```

Added to `toolContext`, `RunParams`, wired in `Run()`.

## Implementation files

| File | Change |
|---|---|
| `agent/tools.go` | Tool def + add to `hotToolNames` |
| `agent/agent.go` | `SendOptionsCallback` type, add to `toolContext`/`RunParams`, `executeTool` dispatch |
| `agent/confirm.go` | `execReplyOptions` function |
| `bot/telegram.go` | `sendOptionsCallback` closure, wire into RunParams |
| `bot/callbacks.go` | Register handler, `handleOptionCallback`, `runOptionFollowUp` |
| `agent_prompt.md` | Tool docs, flow examples, rules section |

## Callback flow

1. Agent calls `reply_options(question, options)`
2. `execReplyOptions` sends keyboard via `sendOptionsCallback`, stores in `pending_confirmations` with `action_type="options"`
3. Agent continues to `reply` + `done` as normal
4. User clicks a button → `handleOptionCallback` fires
5. Handler looks up the pending record, parses the selected index, resolves it
6. Edits message: `"<question> → <selected option>"` (removes buttons)
7. Spawns goroutine: `agent.Run()` with synthetic prompt describing the selection
8. Agent responds based on the choice

## Button data format

Each button's Value is just the index: `"0"`, `"1"`, `"2"`, `"3"`. The handler maps it back to the option label via the stored payload.

## Agent prompt additions needed

- Add to always-available tools list
- Flow example for disambiguation
- Flow example for guided choice
- Rules: use for genuine choices (not when you already know the answer), max 4 options, keep labels short

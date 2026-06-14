---
title: "Gmail Integration — Email Triage via Worker Agent"
status: in-progress
created: 2026-06-14
updated: 2026-06-14
category: feature
priority: high
---

# Plan: Gmail Integration — Email Triage via Worker Agent

Give Mira read-only access to a Gmail account so she can search, view, and
summarize emails. Emails stay on Google's servers — no local storage, no
email server. The worker agent handles triage; the driver dispatches via
`send_task` with a new synchronous (`wait`) mode.

**Tracking:** (GH issue TBD)

---

## Problem

Autumn has all email (Proton + 2 Gmail accounts) forwarding to a single
Gmail triage account. Currently there's no way for Mira to access, search,
or summarize emails. The goal is a lightweight integration that:

- Connects to Gmail via API (read-only, no download/storage)
- Lets the worker agent search and read emails using Gmail's native search
- Supports both proactive scheduled triage and on-demand "check my email"
- Includes a synchronous dispatch mode so the driver can wait for results
- Is testable via sims with prefilled fake emails

**Non-goals:**
- Sending, drafting, or deleting emails
- Downloading or caching email content locally
- Semantic/embedding search (Gmail's built-in search is sufficient)
- IMAP/SMTP — API only

---

## Design

### Architecture

```
User: "check my email"
  │
  ▼
Driver agent
  │  send_task(target="worker", task_type="email_check",
  │            note="check for anything urgent", wait=true)
  ▼
Worker agent (Qwen3, low tier)
  │  search_emails(query="is:unread newer_than:1d")
  │  read_email(id="msg_abc123")
  │  think("3 emails look important...")
  │  done(summary="3 urgent emails: ...")
  ▼
Worker result returns as tool response to driver
  │
  ▼
Driver: reply("You've got 3 things worth looking at...")
```

### Auth: OAuth2 Without a Browser Flow

Gmail API requires user-level OAuth2. Since Mira runs on a headless VPS,
the auth flow uses Google's OAuth Playground for one-time token generation:

1. Create OAuth credentials in Google Cloud Console (Desktop app type)
2. Enable Gmail API in the project
3. Use OAuth Playground to authorize `gmail.readonly` scope
4. Copy `client_id`, `client_secret`, and `refresh_token` to config.yaml
5. Go client auto-refreshes access tokens from the refresh token

**Important:** Push the Cloud Console app to "Production" status (just a
form, no review for <100 users) so refresh tokens don't expire after 7 days.

### Config

```yaml
gmail:
  client_id: "xxx.apps.googleusercontent.com"
  client_secret: "xxx"
  refresh_token: "xxx"
  account: "triage@gmail.com"    # display only, for prompt context
  max_results: 20                # default page size for list/search
```

### Gmail Package (`gmail/`)

Single owner of all Gmail API interaction. Follows the Bridge interface
pattern established by `calendar/`:

```go
// Bridge is the contract for email access — production uses the
// Gmail API, sims use a fake in-memory implementation.
type Bridge interface {
    Search(ctx context.Context, query string, page int) ([]MessageSummary, error)
    Read(ctx context.Context, id string) (*Message, error)
}

type MessageSummary struct {
    ID      string
    From    string
    Subject string
    Snippet string
    Date    time.Time
    Unread  bool
}

type Message struct {
    MessageSummary
    Body string   // text/plain preferred, HTML stripped as fallback
}
```

- `APIBridge` — production implementation using `google.golang.org/api/gmail/v1`
- `FakeBridge` — in-memory implementation for sims, seeded from YAML

### Worker Tools

Two tools, registered in the worker agent's tool context:

| Tool | Parameters | Returns |
|------|-----------|---------|
| `search_emails` | `query` (string, Gmail search syntax), `page` (int, default 1) | List of MessageSummary (ID, from, subject, snippet, date, unread) |
| `read_email` | `id` (string, message ID from search results) | Full message (headers + plain text body) |

Both tools route through `gmail.Bridge`, injected via `tools.Context`.

### Synchronous `send_task` Extension

Add a `wait` boolean parameter to `send_task`:

- `wait=false` (default): current fire-and-forget behavior
- `wait=true`: handler calls `workeragent.RunWorker()` directly (not via
  goroutine callback), returns `WorkerResult.Summary` as the tool response

This is a small change: the handler already has both patterns available
(production = async, sim = sync). The `wait` param just lets the driver
choose at call time.

Changes:
- `tools/send_task/tool.yaml` — add `wait` boolean parameter
- `tools/send_task/handler.go` — branch on `wait` in `handleWorker()`
- `tools.Context` — add `WorkerCallbackSync` field (returns WorkerResult)

### Task Type

```
workeragent/tasks/email_check/
├── meta.yaml       # model_tier: low
└── prompt.md       # email triage prompt
```

The prompt instructs the worker to:
1. Search for unread/recent emails (or follow the driver's specific query)
2. Scan subjects and snippets to identify what's important
3. Read full body only for emails that look actionable
4. Produce a concise summary grouped by urgency
5. Call `done` with the summary

### Sim Infrastructure

Follow the calendar bridge pattern:

**FakeBridge** (`gmail/fake.go`):
- In-memory slice of `Message` structs
- `Search()` filters by substring match on from/subject/body (mirrors
  Gmail search basics — `from:`, `subject:`, `is:unread`)
- `Seed([]Message)` populates the fake inbox

**YAML seed field:**
```yaml
seed_emails:
  - id: "msg-001"
    from: "mom@example.com"
    subject: "Dinner Sunday?"
    snippet: "Hey sweetie, are you free for dinner..."
    body: "Hey sweetie, are you free for dinner this Sunday? Dad's grilling."
    date: "2026-06-14T10:30:00-04:00"
    unread: true
  - id: "msg-002"
    from: "noreply@github.com"
    subject: "[her-go] PR #87 merged"
    snippet: "Your pull request has been merged..."
    body: "Pull request #87 'Gmail integration' has been merged into main."
    date: "2026-06-14T09:15:00-04:00"
    unread: false
```

**Sim harness changes (`cmd/sim.go`):**
- Parse `seed_emails` from suite YAML
- Populate FakeBridge before message loop
- Inject FakeBridge into tools.Context (new field: `GmailBridge`)
- Capture email interactions in sim report

---

## Phases

### Phase 1: Gmail Package + Bridge Interface

- [ ] `gmail/bridge.go` — Bridge interface, types (MessageSummary, Message)
- [ ] `gmail/api.go` — APIBridge implementation (Google API client, token refresh, MIME parsing)
- [ ] `gmail/fake.go` — FakeBridge implementation (in-memory, substring search)
- [ ] `gmail/parse.go` — MIME body extraction (text/plain preferred, HTML→text fallback)
- [ ] `config/config.go` — add `GmailConfig` struct + field on Config
- [ ] `config.yaml.example` — add gmail section

### Phase 2: Worker Tools

- [ ] `tools/search_emails/tool.yaml` + `handler.go` — search/list emails via Bridge
- [ ] `tools/read_email/tool.yaml` + `handler.go` — read single email via Bridge
- [ ] `workeragent/worker.go` — add blank imports for new tools
- [ ] `tools.Context` — add `GmailBridge` field

### Phase 3: Synchronous send_task

- [ ] `tools/send_task/tool.yaml` — add `wait` boolean parameter
- [ ] `tools/send_task/handler.go` — synchronous dispatch when `wait=true`
- [ ] `tools.Context` — add `WorkerCallbackSync` (returns WorkerResult)
- [ ] `cmd/run.go` — wire up sync callback alongside existing async one

### Phase 4: Task Type + Prompt

- [ ] `workeragent/tasks/email_check/meta.yaml` — model_tier: low
- [ ] `workeragent/tasks/email_check/prompt.md` — email triage instructions

### Phase 5: Sim Infrastructure + Test Suite

- [ ] `cmd/sim.go` — parse `seed_emails`, populate FakeBridge, inject into context
- [ ] `sims/email-triage.yaml` — reference sim with seeded emails, multi-turn scenarios
- [ ] Wire FakeBridge in sim harness (same pattern as calendar FakeBridge)

### Phase 6: Production Wiring

- [ ] `cmd/run.go` — initialize gmail.APIBridge from config, inject into tools.Context
- [ ] OAuth2 setup documentation in README or docs/

---

## Files That Would Change

| File | Change |
|------|--------|
| `gmail/bridge.go` | **new** — Bridge interface + types |
| `gmail/api.go` | **new** — APIBridge (Google API client) |
| `gmail/fake.go` | **new** — FakeBridge (sims) |
| `gmail/parse.go` | **new** — MIME body extraction |
| `tools/search_emails/tool.yaml` | **new** — tool definition |
| `tools/search_emails/handler.go` | **new** — handler |
| `tools/read_email/tool.yaml` | **new** — tool definition |
| `tools/read_email/handler.go` | **new** — handler |
| `tools/send_task/tool.yaml` | add `wait` parameter |
| `tools/send_task/handler.go` | add sync dispatch path |
| `tools/context.go` | add GmailBridge + WorkerCallbackSync fields |
| `workeragent/worker.go` | blank imports for email tools |
| `workeragent/tasks/email_check/` | **new** — task type |
| `config/config.go` | add GmailConfig |
| `config.yaml.example` | add gmail section |
| `cmd/run.go` | wire APIBridge + sync callback |
| `cmd/sim.go` | seed_emails parsing + FakeBridge injection |
| `sims/email-triage.yaml` | **new** — reference sim |

---

## Design Decisions

| Decision | Choice | Alternative | Why |
|----------|--------|-------------|-----|
| Auth flow | OAuth Playground (manual token) | In-app browser flow | Headless VPS, one-time setup |
| Email storage | None (API-only) | Local cache/DB | Privacy, simplicity, no email server |
| Search | Gmail native search syntax | Embedding-based semantic | Emails stay on Google, can't embed |
| Bridge pattern | Interface (API + Fake) | Direct API calls | Testability via sims, proven pattern |
| Worker dispatch | sync `wait` param | Always async | Driver needs inline results for "check my email" |
| Tool scope | Worker-only | Driver-accessible | Keeps driver context clean, worker handles messy MIME |
| MIME parsing | text/plain preferred, strip HTML | Always render HTML | Simpler, LLM doesn't need formatting |
| Gmail scope | `gmail.readonly` | Full access | Mira reads, never sends/deletes |

---

## Known Limitations

- **Gmail search only** — no semantic/embedding search since emails aren't stored locally
- **Single account** — designed for one triage account (all forwarding goes there)
- **Token refresh** — if the Google Cloud app stays in "Testing" mode, tokens expire after 7 days. Must push to "Production" for long-lived tokens
- **MIME complexity** — multipart emails with attachments, inline images, etc. may not parse cleanly in all cases. We extract text/plain when available and strip HTML as fallback. Attachments are listed by name but not downloaded
- **Rate limits** — Gmail API per-user rate limits exist but are generous for single-user personal use

---
title: "Always-On Infrastructure: CF Worker Routing + Mac Mini Deployment"
status: planning
created: 2026-04-29
updated: 2026-04-29
category: infrastructure
priority: high
phases:
  - webhook-mode
  - cf-worker-router
  - cloudflare-tunnels
  - dev-mode-tooling
  - self-update-command
  - d1-shared-state
related:
  - docs/migration-postgres.md
---

# Always-On Infrastructure Plan

> The goal: her runs 24/7 on the Mac Mini. Development continues on the MacBook with full local TUI. Both machines can coexist without stepping on each other's Telegram messages. Deployment is a single Telegram command.

---

## Problem Statement

Right now the bot only works when actively running, and can only run on one machine at a time. Two blockers make this hard to solve:

**1. Long polling has no concept of a primary instance.**
The bot currently uses `tele.LongPoller` — it calls Telegram every 10 seconds asking for messages. If two instances poll the same bot token simultaneously, Telegram distributes each message to whichever instance happened to ask first. This is random. There is no way to intercept, route, or control delivery without changing the transport.

**2. There is no deployment mechanism.**
Updating a binary running on the Mac Mini requires either being physically present or SSHing in. The goal is to never need SSH for routine operations — Telegram is the only interface when away from the machines.

---

## Solution: Cloudflare Worker as Routing Layer

Switch from long polling to webhooks. Register a **Cloudflare Worker** as the single Telegram webhook endpoint. The Worker receives all updates and routes them to the appropriate backend: the Mac Mini for normal operation, or the MacBook for active dev sessions.

```
Telegram
  │
  ▼ POST /webhook
┌─────────────────────────────────┐
│  Cloudflare Worker              │
│  ┌──────────────────────────┐   │
│  │  KV: dev_mode_active?    │   │
│  │  no  → prod tunnel URL   │   │
│  │  yes → dev tunnel URL    │   │
│  └──────────────────────────┘   │
│  ctx.waitUntil(forward)         │  ← fire-and-forget; Telegram gets 200 immediately
│  return 200 OK                  │
└─────────────────────────────────┘
         │                    │
         ▼                    ▼
  Cloudflare Tunnel    Cloudflare Tunnel
  (Mac Mini)           (MacBook, ephemeral)
         │                    │
         ▼                    ▼
  her webhook server   her webhook server
  port 8765 (prod)     port 8765 (dev)
         │
         ▼
  her.db (SQLite)      her-dev.db (SQLite)
```

### Why This Works

Webhooks make Telegram the *caller*, not the receiver. There is exactly one registered webhook URL at a time — the Worker — and it decides where to forward. Long polling must be removed entirely; Telegram only supports one delivery method at a time.

The Worker forwards and immediately returns `200 OK` to Telegram using `ctx.waitUntil()`. This decouples Telegram's timeout from backend latency. If the backend is slow, Telegram doesn't care — it already got its `200`.

### Why Cloudflare Tunnels Are Needed (and Why Your Phone Isn't Involved)

Your phone is just a Telegram client — it sends messages to Telegram's servers over normal Telegram, and receives replies the same way. The phone never touches the Mac Mini or the CF Worker directly. No changes needed there.

The tunnel is needed for a different reason: the CF Worker needs to forward Telegram updates to the Mac Mini, and the Mac Mini is behind your home NAT. It has a private IP (`192.168.1.x`) that nothing on the open internet can reach by default. The tunnel solves this by having the Mac Mini establish an outbound connection to Cloudflare's edge, which creates a stable inbound path — essentially port forwarding, but handled by Cloudflare with a permanent HTTPS URL regardless of your home IP changing.

Only the two machines that act as webhook receivers need a tunnel:
- **Mac Mini** — permanent tunnel, always running as a launchd service
- **MacBook** — temporary tunnel, only active during `her dev` sessions

### KV Routing Logic

Two KV keys control routing:

| Key | Value | Notes |
|---|---|---|
| `dev_mode_active` | `"true"` or absent | Set on MacBook dev session start, cleared on stop |
| `dev_instance_url` | `"https://abc.trycloudflare.com"` | Set alongside dev_mode_active |

The prod URL is a fixed Worker environment variable — it never changes because the Mac Mini's tunnel URL is permanent.

If `dev_mode_active` is set but `dev_instance_url` is missing or the forward fails, the Worker falls back to prod. Stale dev sessions (MacBook crashed without cleanup) are handled by a `dev_session_heartbeat` timestamp key — if it's older than 5 minutes, the Worker ignores dev mode and routes to prod.

---

## Phases

### Phase 1 — Webhook Mode in Bot

**What changes:** `bot/telegram.go` currently hardcodes `tele.LongPoller` and unconditionally calls `RemoveWebhook(true)`. This phase makes the bot respect `cfg.Telegram.Mode`.

**Changes needed:**
- `bot/telegram.go`: conditionally use `tele.Webhook{Listen: ":8765", ...}` when `mode == "webhook"`, skip `RemoveWebhook`
- `bot/telegram.go`: don't call `RemoveWebhook(true)` in webhook mode (it would unregister the webhook the CF Worker registered)
- The telebot library handles the HTTP server and update parsing automatically in webhook mode — it just needs a `Listen` address and an optional secret token
- `config.yaml.example`: add `webhook_port: 8765` and `webhook_secret` fields

The webhook secret is a random string you set once. Telegram sends it in the `X-Telegram-Bot-Api-Secret-Token` header with every update. The CF Worker validates it before forwarding — drops any request that doesn't match. This is the primary security layer against random internet traffic.

**Dev note:** The `webhook_url` config field already exists but is unused. `webhook_port` is new. The URL itself is registered externally (either by the CF Worker setup script or via BotFather) — the bot only needs to know which port to listen on.

### Phase 2 — Cloudflare Worker

**What gets built:** A small JavaScript Worker (`worker/index.js`) that lives in this repo in a `worker/` directory.

```
worker/
├── index.js          ← routing logic
├── wrangler.toml     ← CF config: KV bindings, env vars, route
└── README.md         ← how to deploy (wrangler deploy)
```

**Worker logic (simplified):**
```javascript
export default {
  async fetch(request, env, ctx) {
    // Validate webhook secret
    const secret = request.headers.get('X-Telegram-Bot-Api-Secret-Token');
    if (secret !== env.WEBHOOK_SECRET) return new Response('', { status: 403 });

    // Determine target
    const devActive = await env.HER_KV.get('dev_mode_active');
    const devUrl = devActive ? await env.HER_KV.get('dev_instance_url') : null;
    const devHeartbeat = devActive ? await env.HER_KV.get('dev_session_heartbeat') : null;
    const devStale = !devHeartbeat || (Date.now() - parseInt(devHeartbeat)) > 5 * 60 * 1000;
    const targetBase = (devActive && devUrl && !devStale) ? devUrl : env.PROD_URL;

    // Forward fire-and-forget; return 200 to Telegram immediately
    const body = await request.arrayBuffer();
    ctx.waitUntil(fetch(targetBase + '/webhook', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'X-Telegram-Bot-Api-Secret-Token': env.WEBHOOK_SECRET,
      },
      body,
    }));

    return new Response('OK', { status: 200 });
  },
};
```

**wrangler.toml config:**
```toml
name = "her-router"
main = "worker/index.js"
compatibility_date = "2026-01-01"

kv_namespaces = [
  { binding = "HER_KV", id = "<kv-namespace-id>" }
]

[vars]
PROD_URL = "https://her.yourdomain.com"
# WEBHOOK_SECRET stored as a Wrangler secret (not in TOML)
```

### Phase 3 — Cloudflare Tunnels

**Mac Mini (permanent tunnel):**
```bash
# One-time setup
cloudflared tunnel create her-prod
cloudflared tunnel route dns her-prod her.yourdomain.com

# Config: ~/.cloudflared/config.yml
tunnel: her-prod
credentials-file: /Users/autumn/.cloudflared/<tunnel-id>.json
ingress:
  - hostname: her.yourdomain.com
    service: http://localhost:8765
  - service: http_status:404
```

Run as a launchd service (same pattern as the bot itself — `cmd/setup.go` could generate the plist).

**MacBook (ephemeral dev tunnel):**
No setup needed. Use `cloudflared tunnel --url http://localhost:8765` — Cloudflare assigns a random `*.trycloudflare.com` URL. The dev mode command captures this URL and puts it in KV.

**Open question:** Does your domain's nameservers point to Cloudflare? This is required for `cloudflared tunnel route dns` to work. If not, a `*.trycloudflare.com` URL can also be used as the permanent Mac Mini URL — it's less clean but doesn't require a domain.

### Phase 4 — Dev Mode Tooling

**What gets built:** A `cmd/dev.go` command that manages the MacBook dev session lifecycle.

`her dev` does:
1. Starts `cloudflared tunnel --url http://localhost:8765` as a subprocess, captures the assigned URL from stdout
2. POSTs to CF Workers API: sets `dev_mode_active=true`, `dev_instance_url=<url>`, `dev_session_heartbeat=<now>`
3. Starts the bot in dev mode using `config.dev.yaml` (separate DB path `./her-dev.db`, same API keys)
4. Sends a background goroutine that refreshes `dev_session_heartbeat` in KV every 2 minutes (keeps session alive)
5. On shutdown (Ctrl+C → `signal.Notify`): POSTs to CF Workers API to delete the KV keys, then exits

`config.dev.yaml` example:
```yaml
telegram:
  token: "${TELEGRAM_BOT_TOKEN}"  # same token, the router handles separation
  mode: "webhook"
  webhook_port: 8765
memory:
  db_path: "./her-dev.db"
# everything else inherited from config.yaml
```

The CF Workers API calls use `CLOUDFLARE_ACCOUNT_ID` and `CLOUDFLARE_API_TOKEN` from environment (or config). These are only needed on the MacBook for dev session management — the Mac Mini doesn't need them.

**What the MacBook dev experience looks like:**
```bash
her dev        # sets KV flags, starts tunnel, starts bot
               # TUI available as normal
# Ctrl+C      # clears KV flags, bot stops, Mac Mini takes over immediately
```

### Phase 5 — `/update` Command

**What it does:** Self-updating bot via Telegram command. Designed for the Mac Mini instance only (dev mode should ignore it or route to a dev-specific handler).

Flow:
1. `/update` received on Mac Mini instance
2. Reply: "Pulling changes..."
3. `git -C <repo_dir> pull origin main` — capture output
4. If pull says "Already up to date": reply and stop
5. If pull fails: reply with error, stop
6. Reply: "Building..."
7. `go build -o <exe>.next .` in repo dir — capture stderr
8. If build fails: reply with compiler errors (truncated to Telegram's 4096 char limit), stop. Bot keeps running with old binary
9. `cp <exe> <exe>.backup` — keep rollback copy
10. `mv <exe>.next <exe>` — atomic rename
11. Write `her.update_pending` flag file with timestamp + "Updated successfully" message text
12. `launchctl kickstart -k <service-label>` — kills current process, launchd starts fresh binary
13. On next startup: check for `her.update_pending`, read message, delete flag, send message to owner_chat

**Rollback:** Manual. If new binary fails to start (crashes immediately), `her.backup` exists. Since you can't send a Telegram message when the bot is down, recovery requires SSH. This is the acceptable tradeoff for v1 — build-time failures are caught before swap (step 8), which is the most common failure mode. Runtime crashes after a successful build are rare if you tested locally first.

The repo path and service label come from config:
```yaml
update:
  repo_path: "/Users/autumn/Developer/her-go"
  service_label: "com.mira.her-go"
```

`com.mira.her-go.plist` already exists in the project root — the service label is already established.

### Phase 6 — Trace Parity with TUI

**Goal:** When running headless on the Mac Mini, Telegram traces should give the same observability the TUI gives during local dev. The trace infrastructure (`Board`, slots, live-editing) already exists and is solid — this phase is about filling the content gaps and handling overflow gracefully.

**What the TUI shows that traces currently don't:**

| TUI section | What's missing from traces |
|---|---|
| Turn header | Per-turn cost, latency, tool count — shown as a summary line in TUI, not written to the `main` trace slot |
| Context box | "N memories retrieved semantically" — a `ContextEvent` exists in the TUI event system but isn't surfaced to the trace Board |
| Reply box | Full token breakdown (prompt + completion + total), cost, latency — TUI renders this as a metrics line; traces get the reply text but not the numbers |
| Persona box | `reflection_triggered`, `rewrite_triggered` events — these go to TUI's persona section but there's no persona trace slot |
| Driver box | Tool calls are already in traces but think/reply/done rendering could be richer (TUI shows truncated thought text inline) |

Memory and mood slots already exist and are wired up — those are fine.

**New trace slot: `persona`**

Register a `persona` stream in `trace/registry.go` (Order 150, between main and memory). Wire it up in `bot/run_agent.go` alongside the existing `traceCallback`, `memoryTraceCallback`, `moodTraceCallback`. Persona events (reflection, rewrite, trait shift) write into this slot.

**Enriching the `main` slot**

The turn summary line (cost · latency · N tools) should be appended to the main slot when the turn completes — same data the TUI puts in the turn header. Context retrieval count should appear at the top of main when facts are injected.

**Pagination for overflow**

The trace Board live-edits a single Telegram message per turn. Telegram's hard limit is 4096 characters. Heavy turns (long think steps, multiple web searches, memory writes) can exceed this silently.

`bot/paginate.go` already has `b.sendPaginated(c, text)` — one call, handles page splitting, page footer, and ◀/▶ inline buttons automatically. It's used by `/facts` today.

Approach:
- Keep the live-edited single message during the turn (in-progress traces stream updates — splitting mid-stream is disruptive)
- When the turn completes: if the final Board snapshot exceeds ~3800 chars, replace the live message with a paginated send via `b.sendPaginated`
- Add a `/lasttrace` command that re-sends the last turn's full trace through `sendPaginated` on demand — useful when traces are disabled globally but you want the detail for one specific turn

**`/lasttrace` command**

Store the last completed turn's full Board snapshot in the `Bot` struct (a single `string` field, replaced each turn). `/lasttrace` calls `b.sendPaginated(c, b.lastTraceSnapshot)`. If traces are globally disabled, this is the only way to get the detail — opt-in per-turn observability.

### Phase 7 — D1 Shared State (Dependency)

This phase enables the MacBook dev instance to share memory/persona with the Mac Mini prod instance. Without it, `her-dev.db` starts empty and doesn't know about real memories — which is fine for testing but means the dev bot "doesn't know you."

**Scope:** See `docs/migration-postgres.md` for the full `RemoteStore` interface design. The D1 variant of that plan replaces Postgres with D1 REST API calls. The split:

| Data | Storage |
|---|---|
| Messages, facts (text), persona, reflections | D1 (shared, accessible from both machines) |
| Fact embeddings | Local SQLite + sqlite-vec (machine-local, not shared) |
| Metrics, PII vault, scheduled tasks | Local SQLite only |

Embeddings stay local because: (a) Vectorize is not needed at this scale, (b) `nomic-embed-text` is cheap to run locally, (c) each machine can rebuild its own embedding index from D1 fact text if needed.

The embedding index being machine-local means semantic search results may vary slightly between dev and prod during the transition period. Acceptable.

This phase is a significant undertaking and should be planned separately. The existing `docs/migration-postgres.md` covers the `Store` interface extraction (Phase 4 of that plan) which is a prerequisite regardless of whether the backend is Postgres or D1.

---

## Startup Behavior (After All Phases)

When the Mac Mini bot starts (fresh boot or after `/update` restart):
1. Check for `her.update_pending` flag → send update confirmation message if found
2. Send "her online" to `owner_chat` (helps detect if Mac Mini rebooted unexpectedly)
3. Start HTTP server in webhook mode on configured port
4. Cloudflare Tunnel is already running as a launchd service, forwarding traffic

When `her dev` starts on MacBook:
1. CF Worker begins routing dev traffic to MacBook
2. Mac Mini bot continues running, receives nothing until dev session ends
3. MacBook has TUI, full observability
4. Ctrl+C: KV cleared, Mac Mini immediately resumes receiving all traffic

Note: `her dev` does not exist yet — `cmd/dev.go` is new code. The existing `her run` continues to work as-is for local dev before Phase 4 is implemented.

---

## Prerequisites

- [ ] Cloudflare account with Workers and KV access (already have Workers plan)
- [ ] Domain with nameservers on Cloudflare OR accept `*.trycloudflare.com` URLs
- [ ] `cloudflared` CLI installed on both machines (`brew install cloudflare/cloudflare/cloudflared`)
- [ ] `wrangler` CLI installed for Worker deployment (`npm install -g wrangler`)
- [ ] Mac Mini has the git repo checked out (not just the binary) — required for Phase 5 `/update`
- [ ] Mac Mini has Go installed — required for Phase 5

---

## What This Does Not Solve

- **Voice on Mac Mini is fine** — Piper TTS runs on Linux and macOS, `ffmpeg` is installable anywhere. The only voice loss is Parakeet STT (Apple Silicon / MLX only). If the Mac Mini is Apple Silicon, both work. If it's Intel Mac, STT breaks. Check which Mac Mini you have.
- **Calendar bridge** — EventKit stays macOS-only. Since both machines are Macs, calendar continues to work on both.
- **Remote SSH for emergencies** — Tailscale is still worth installing on the Mac Mini for the rare case where the bot is down and you need to recover. This is the break-glass option, not the normal workflow.

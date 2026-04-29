# her-router

Cloudflare Worker that routes Telegram webhook updates to the correct backend instance.

## How it works

```
Telegram → POST /webhook → CF Worker → KV routing check → Cloudflare Tunnel → her
```

The Worker validates the webhook secret, checks KV for an active dev session, and forwards. Returns 200 to Telegram immediately — backend processing happens asynchronously.

## Setup

```bash
cd worker

# 1. Create the KV namespace
npx wrangler kv namespace create HER_KV
# Copy the id into wrangler.toml

# 2. Set the webhook secret (must match config.yaml's webhook_secret)
npx wrangler secret put WEBHOOK_SECRET

# 3. Update PROD_URL in wrangler.toml with your tunnel URL

# 4. Deploy
npx wrangler deploy

# 5. Register the Worker URL as Telegram's webhook
curl "https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/setWebhook?url=https://her-router.<your-subdomain>.workers.dev/webhook&secret_token=${WEBHOOK_SECRET}"
```

## KV routing keys

| Key | Value | Set by |
|-----|-------|--------|
| `dev_mode_active` | `"true"` or absent | `her dev` on MacBook |
| `dev_instance_url` | tunnel URL | `her dev` on MacBook |
| `dev_session_heartbeat` | Unix ms timestamp | `her dev` heartbeat goroutine (every 2min) |

If `dev_mode_active` is set but the heartbeat is older than 5 minutes, the Worker falls back to prod.

## Local testing

```bash
npx wrangler dev
# Then POST to http://localhost:8787/webhook with the secret header
```

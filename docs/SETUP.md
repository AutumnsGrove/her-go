# Setup Guide

This guide covers detailed setup for specific features beyond the basic config. For the quick start, see the main [README](../README.md).

## Table of Contents

- [Voice Setup](#voice-setup)
- [Calendar Integration](#calendar-integration)
- [Cross-Machine Sync (D1)](#cross-machine-sync-d1)
- [Deployment (Background Service)](#deployment-background-service)
- [Development Mode](#development-mode)

---

## Voice Setup

Mira supports both voice input and output, running entirely on your machine for privacy.

### Speech-to-Text (Parakeet)

**Recommended for macOS with Apple Silicon.** Parakeet uses the MLX framework for fast on-device transcription.

1. **Install dependencies** (handled by `her setup`):
   ```bash
   # Install UV (Python package manager)
   curl -LsSf https://astral.sh/uv/install.sh | sh
   
   # Install parakeet-mlx-fastapi
   uv tool install git+https://github.com/yashhere/parakeet-mlx-fastapi.git
   ```

2. **Enable in config**:
   ```yaml
   voice:
     enabled: true
     stt:
       engine: "parakeet"
       base_url: "http://localhost:8765"
       model: "mlx-community/parakeet-tdt-0.6b-v2"
   ```

3. **Start the bot** — the Parakeet sidecar spawns automatically:
   ```bash
   her run
   ```

When you send a voice memo on Telegram, Mira downloads it, transcribes it locally, and processes the text through the normal pipeline.

### Speech-to-Text (Whisper - Remote)

**For VPS deployment or non-Apple platforms.** Uses OpenRouter or OpenAI's Whisper API.

```yaml
voice:
  enabled: true
  stt:
    engine: "whisper"
    base_url: "https://openrouter.ai/api/v1"
    model: "openai/whisper-large-v3-turbo"
    # api_key: ""  # leave empty to use openrouter.api_key
```

### Text-to-Speech (Piper)

**Local, privacy-first TTS with natural voices.**

1. **Download voice models**:
   ```bash
   her setup
   ```
   
   This automatically downloads the default voice (`en_GB-southern_english_female-low`, 22 MB) to `scripts/piper-voices/`. The setup command runs all dependency installs concurrently, so Piper voices download in the background while the binary builds.

2. **Enable in config**:
   ```yaml
   voice:
     tts:
       enabled: true
       engine: "piper"
       model: "en_GB-southern_english_female-low"
       reply_mode: "voice"  # or "match" to reply in same format as input
       speed: 1.0
   ```

3. **Start the bot** — the Piper TTS server spawns automatically on port 8766.

#### Choosing a Different Voice

Piper supports 100+ voices across many languages. Browse them at [rhasspy/piper-voices](https://huggingface.co/rhasspy/piper-voices/tree/main).

**To add a new voice:**

1. Navigate to the voice you want (e.g., `en/en_US/amy/medium/`)
2. Download both files:
   - `en_US-amy-medium.onnx` (model weights)
   - `en_US-amy-medium.onnx.json` (config)
3. Save them to `scripts/piper-voices/`
4. Update `config.yaml`:
   ```yaml
   voice:
     tts:
       model: "en_US-amy-medium"  # filename without .onnx
   ```

**Voice quality tiers:**
- `x_low` — fastest, smallest, robotic
- `low` — good balance (recommended)
- `medium` — better quality, slower
- `high` — best quality, slowest

#### Customizing Speech Pauses

Piper inserts silence at punctuation boundaries. Adjust pause durations in config:

```yaml
voice:
  tts:
    pauses:
      paragraph_ms: 500   # double newlines
      line_ms: 250        # single newlines
      sentence_ms: 75     # . ! ?
      comma_ms: 50        # ,
      semi_ms: 30         # ; :
```

---

## Calendar Integration

**macOS only.** Mira can read and write to Apple Calendar via a Swift EventKit bridge.

### Setup

1. **Build the Swift binary**:
   ```bash
   cd calendar/bridge
   swift build -c release
   ```
   
   This creates `calendar/bridge/.build/release/her-calendar`.

2. **Grant calendar access**:
   - The first time Mira tries to access your calendar, macOS will prompt for permission
   - Go to **System Settings → Privacy & Security → Calendars**
   - Ensure the `her-calendar` binary (or Terminal, if running via `her run`) is checked

3. **Configure calendars** in `config.yaml`:
   ```yaml
   calendar:
     bridge_path: "calendar/bridge/.build/release/her-calendar"
     calendars:
       - "Calendar"   # Default Apple Calendar
       - "Work"
       - "Personal"
     default_calendar: "Calendar"
     default_timezone: "America/New_York"
   ```

4. **Define jobs** (optional, for shift tracking):
   ```yaml
   calendar:
     jobs:
       - name: "Panera"
         address: "123 Main St, City, State 12345"
         default_role: ""
         aliases: ["panera bread"]
   ```

### Available Tools

Once configured, Mira can:
- **`calendar_list`** — Show upcoming events from all monitored calendars
- **`calendar_create`** — Create new events or work shifts
- **`shift_hours`** — Calculate total hours for a job within a date range

Example: *"Show me my schedule for this week"* or *"Log a shift at Panera tomorrow 3-9pm"*.

---

## Cross-Machine Sync (D1)

Share Mira's memory across multiple machines (laptop ↔ VPS) using Cloudflare D1 as a central mirror.

### Architecture

- **Local-first**: All reads/writes go to local SQLite (`her.db`)
- **Async push**: A background carrier goroutine pushes local writes to D1 via an outbox table
- **Pull on demand**: `her sync pull` fetches remote changes from D1 to a new machine

### Setup

1. **Create a D1 database**:
   ```bash
   cd worker
   npx wrangler d1 create her-shared-state
   ```
   
   Copy the `database_id` from the output.

2. **Run migrations**:
   ```bash
   npx wrangler d1 migrations apply her-shared-state --local
   npx wrangler d1 migrations apply her-shared-state --remote
   ```

3. **Add to config** on both machines:
   ```yaml
   cloudflare:
     account_id: "your-account-id"
     api_token: "your-api-token"  # needs D1 + Workers KV write permission
     d1_database_id: "abc123-..."  # from step 1
   ```

4. **Push from primary machine**:
   ```bash
   her sync push
   ```

5. **Pull to secondary machine**:
   ```bash
   her sync pull
   ```

Now both machines will stay in sync. Local writes push to D1 in the background; run `her sync pull` periodically on the secondary machine to fetch updates.

### Monitoring

Check sync status:
```bash
her sync status
```

This shows:
- Outbox queue depth
- Last push timestamp
- Carrier goroutine health

---

## Deployment (Background Service)

Run Mira as a background service using macOS launchd.

### Setup

1. **Create a launch agent**:
   ```bash
   her start
   ```
   
   This generates `~/Library/LaunchAgents/com.mira.her-go.plist` and loads it.

2. **Check status**:
   ```bash
   her status
   ```

3. **View logs**:
   ```bash
   her logs           # tail combined output
   her logs --stderr  # tail error stream only
   her logs --lines 100  # show last 100 lines
   ```

4. **Stop the service**:
   ```bash
   her stop
   ```

The service auto-restarts on crashes and starts at login.

### Deployment with Cloudflare Tunnel

For always-on webhook mode (faster than polling, no local port forwarding):

1. **Install cloudflared**:
   ```bash
   brew install cloudflare/cloudflare/cloudflared
   cloudflared tunnel login
   ```

2. **Create a tunnel**:
   ```bash
   her tunnel setup
   ```
   
   This creates a tunnel, generates credentials, and prompts for your domain.

3. **Update config**:
   ```yaml
   telegram:
     mode: "webhook"
     webhook_url: "https://her.yourdomain.com"
     webhook_port: 8443
     webhook_secret: "your-secret-token"
   
   tunnel:
     name: "her-prod"
     domain: "her.yourdomain.com"
     credentials_file: "/path/to/credentials.json"
   ```

4. **Start the tunnel**:
   ```bash
   her tunnel start
   ```

The tunnel creates a stable HTTPS endpoint that routes to your local webhook server, even behind NAT.

---

## Development Mode

Fast iteration with webhook + live tunnel, no need to restart on code changes.

```bash
her dev
```

This:
1. Spawns a cloudflared tunnel for webhook delivery
2. Registers the webhook URL with Telegram
3. Starts the bot in webhook mode
4. Routes incoming updates via KV → Worker → local webhook server

Press `Ctrl+C` to stop. The tunnel tears down automatically.

**When to use dev mode:**
- Testing webhook-specific features (inline buttons, callback queries)
- Working on worker agent tasks
- Debugging Telegram update handling

**When to use `her run`:**
- Normal development (long-polling is simpler)
- Testing agent prompts or tool behavior
- Working on local-only features (calendar, voice, etc.)

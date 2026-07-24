# TTS Engine Configuration Guide

The bot supports three text-to-speech engines, each with different tradeoffs. This guide shows you how to switch between them and when to use each one.

---

## Quick Comparison

| Engine | Type | Quality | Cost | Latency | Privacy | Voices | Languages |
|--------|------|---------|------|---------|---------|--------|-----------|
| **Piper** | Local | Good | Free | Fast (~3.5s) | Full | 50+ | 20+ |
| **OpenRouter** | Remote | Very Good | $0.62/1M chars | Medium (~4-6s) | API provider | 54 (Kokoro) | 8 |
| **ElevenLabs** | Remote | Excellent | ~$0.30/1K chars | Fast (~2-3s) | API provider | 100+ | 29 |

**TL;DR:**
- **Privacy-first, zero cost**: Use Piper
- **Best quality, don't care about cost**: Use ElevenLabs
- **Good balance, tracked costs**: Use OpenRouter (Kokoro)

---

## Engine 1: Piper (Local, Default)

**Pros:**
- ✅ Completely local — no audio leaves your machine
- ✅ Zero API cost — unlimited usage
- ✅ Fast synthesis (~3.5s for a 22-second paragraph)
- ✅ Automatic startup — bot spawns the sidecar for you
- ✅ RAM-efficient — idle unload frees ~120MB after 5 minutes

**Cons:**
- ❌ Lower quality than cloud TTS (robotic at times)
- ❌ Requires Python + dependencies (see setup below)
- ❌ No emotion/tone variation

**Best for:** Privacy-conscious users, always-on deployments, VPS hosting

### Configuration

```yaml
voice:
  tts:
    enabled: true
    engine: "piper"
    base_url: "http://localhost:8766"
    model: "en_GB-southern_english_female-low"
    voice_id: "en_GB-southern_english_female-low"
    speed: 1.0  # 1.0 = normal, 0.8 = slower, 1.2 = faster
    reply_mode: "voice"  # "voice" or "match"
    idle_unload_sec: 300  # unload after 5 min idle to save RAM
    pauses:
      paragraph_ms: 500
      line_ms: 250
      sentence_ms: 75
      comma_ms: 50
      semi_ms: 30
```

### Available Voices

Piper includes 50+ voices across 20+ languages. Browse the full list:
- [Piper Voice Samples](https://rhasspy.github.io/piper-samples/)

Common choices:
- `en_GB-southern_english_female-low` — British English, warm
- `en_US-amy-low` — American English, neutral
- `en_US-lessac-medium` — American English, clear (higher quality, more RAM)

### Setup (First Time)

```bash
# Install Python dependencies
pip install piper-tts

# The bot spawns the sidecar automatically when tts.enabled = true
# No manual startup needed!
```

---

## Engine 2: OpenRouter (Remote, Kokoro)

**Pros:**
- ✅ Better quality than Piper
- ✅ Cost tracking built-in — shows up in `her usage`
- ✅ 54 voices across 8 languages (American/British English, Spanish, French, Hindi, Italian, Japanese, Portuguese, Chinese)
- ✅ Fast synthesis (~4-6s typical)
- ✅ Ranked #1 on TTS Arena leaderboard (Jan 2026)
- ✅ No local dependencies — works on any platform

**Cons:**
- ❌ Costs $0.62 per 1 million input characters (~$0.0006 per 1K chars)
- ❌ Requires internet connection
- ❌ Audio goes through OpenRouter's API

**Best for:** Users who want better quality than Piper with transparent cost tracking

### Configuration

```yaml
voice:
  tts:
    enabled: true
    engine: "openrouter"
    base_url: "https://openrouter.ai/api/v1"
    model: "hexgrad/kokoro-82m"
    voice_id: "af_sarah"  # see voice list below
    api_key: ""  # leave empty to use openrouter.api_key
    reply_mode: "voice"
```

### Available Voices (Kokoro 54)

Kokoro includes 54 preset voices organized by language and gender:

**American English (af_*):**
- `af_sarah`, `af_sky`, `af_nicole`, `af_jessica`, `af_emma`, `af_bella`
- `am_adam`, `am_michael`, `am_eric`

**British English (bf_*):**
- `bf_lily`, `bf_isabella`, `bf_grace`, `bf_emma`
- `bm_george`, `bm_lewis`, `bm_daniel`

**Other Languages:**
- Spanish: `sf_*`, `sm_*`
- French: `ff_*`, `fm_*`
- Hindi: `hf_*`, `hm_*`
- Italian: `if_*`, `im_*`
- Japanese: `jf_*`, `jm_*`
- Portuguese: `pf_*`, `pm_*`
- Chinese: `cf_*`, `cm_*`

Full list: https://openrouter.ai/hexgrad/kokoro-82m

### Cost Tracking

OpenRouter TTS costs are automatically tracked and appear in:
- `her usage` — CLI command showing cost breakdown
- Metrics dashboard — cost per turn, per agent role
- Sim reports — TTS costs broken out separately

---

## Engine 3: ElevenLabs (Remote, Premium)

**Pros:**
- ✅ Best quality — natural emotion, tone variation, breathing
- ✅ Fastest synthesis (~2-3s)
- ✅ 100+ ultra-realistic voices
- ✅ 29 languages
- ✅ Emotion/intensity controls (via model parameter)

**Cons:**
- ❌ Most expensive (~$0.30 per 1K characters)
- ❌ No cost tracking via API — monitor via ElevenLabs dashboard
- ❌ Free tier limited to 10K chars/month
- ❌ Requires separate API key

**Best for:** Production apps where voice quality is critical

### Configuration

```yaml
voice:
  tts:
    enabled: true
    engine: "elevenlabs"
    voice_id: "21m00Tcm4TlvDq8ikWAM"  # Rachel (example)
    model: "eleven_flash_v2_5"  # low latency
    api_key: "${ELEVENLABS_API_KEY}"
    reply_mode: "voice"
```

### Getting a Voice ID

1. Browse voices at https://elevenlabs.io/voice-library
2. Click a voice you like
3. Copy the `voiceId` from the URL:
   ```
   https://elevenlabs.io/voice-library/rachel/21m00Tcm4TlvDq8ikWAM
                                                   ^^^^^^^^^^^^^^^^^^
                                                   This is the voice_id
   ```

### Available Models

- `eleven_flash_v2_5` — Low latency, good for chat (default)
- `eleven_multilingual_v2` — Best quality, supports 29 languages
- `eleven_turbo_v2_5` — Fastest, lowest cost

---

## Automatic Fallback

You can configure a fallback engine that kicks in if the primary fails (API error, network issue, quota exhausted). Fallback only works with Piper as the secondary — it's free and always available.

### Example: ElevenLabs → Piper Fallback

```yaml
voice:
  tts:
    enabled: true
    engine: "elevenlabs"
    voice_id: "21m00Tcm4TlvDq8ikWAM"
    model: "eleven_flash_v2_5"
    api_key: "${ELEVENLABS_API_KEY}"
    reply_mode: "voice"
    
    # If ElevenLabs fails, automatically retry with Piper
    fallback:
      engine: "piper"
      base_url: "http://localhost:8766"
      model: "en_GB-southern_english_female-low"
      voice_id: "en_GB-southern_english_female-low"
      idle_unload_sec: 300  # keep this >0 so fallback sidecar isn't always resident
```

### Example: OpenRouter → Piper Fallback

```yaml
voice:
  tts:
    enabled: true
    engine: "openrouter"
    base_url: "https://openrouter.ai/api/v1"
    model: "hexgrad/kokoro-82m"
    voice_id: "af_sarah"
    api_key: ""
    reply_mode: "voice"
    
    fallback:
      engine: "piper"
      base_url: "http://localhost:8766"
      model: "en_GB-southern_english_female-low"
      voice_id: "en_GB-southern_english_female-low"
```

**How it works:**
1. Primary engine tries first
2. If it fails with any error, fallback engine tries
3. Logs warning: `"primary TTS engine failed, falling back"`
4. Voice memos continue working seamlessly

---

## Switching Engines

### Step 1: Update config.yaml

Edit `config.yaml` and change the `engine` field plus engine-specific settings.

### Step 2: Restart the bot

```bash
# If running via systemd (production)
systemctl --user restart her-go

# If running directly
./her run
```

### Step 3: Test

Send a voice memo to the bot. It should reply with voice using the new engine.

---

## Reply Modes

Both engines support two reply modes:

```yaml
reply_mode: "voice"  # Always reply with voice (default)
```
- Bot always sends voice memos back, even if you send text

```yaml
reply_mode: "match"  # Mirror input format
```
- You send voice → bot replies with voice
- You send text → bot replies with text

---

## Cost Comparison Examples

**100 messages/day, average 150 characters per reply:**

| Engine | Daily Chars | Daily Cost | Monthly Cost |
|--------|-------------|------------|--------------|
| Piper | 15,000 | $0.00 | $0.00 |
| OpenRouter (Kokoro) | 15,000 | ~$0.009 | ~$0.27 |
| ElevenLabs | 15,000 | ~$4.50 | ~$135.00 |

**1,000 messages/day (heavy usage):**

| Engine | Daily Chars | Daily Cost | Monthly Cost |
|--------|-------------|------------|--------------|
| Piper | 150,000 | $0.00 | $0.00 |
| OpenRouter (Kokoro) | 150,000 | ~$0.09 | ~$2.70 |
| ElevenLabs | 150,000 | ~$45.00 | ~$1,350.00 |

*Note: OpenRouter costs are tracked automatically and shown in `her usage`. ElevenLabs costs must be monitored via their dashboard.*

---

## Troubleshooting

### Piper: "TTS server returned 500"

The Piper sidecar failed to synthesize. Check:
1. Is the sidecar running? Look for `http://localhost:8766/healthz`
2. Is the model downloaded? First synthesis downloads it automatically
3. Check `logs/tts_server.log` for Python errors

### OpenRouter: "TTS request failed: 401"

Invalid API key. Check:
1. Is `openrouter.api_key` set in config.yaml?
2. Or is `tts.api_key` set explicitly?
3. Is the key valid? Test at https://openrouter.ai

### ElevenLabs: "ElevenLabs returned 401"

Invalid API key. Check:
1. Is `tts.api_key` set correctly?
2. Is the key still valid? Check https://elevenlabs.io/subscription

### ElevenLabs: "ElevenLabs returned 429"

Quota exhausted. You've hit your monthly character limit. Either:
1. Upgrade your ElevenLabs plan
2. Add a Piper fallback (see above)
3. Wait until next month for quota reset

---

## Recommendations by Use Case

**Personal assistant (privacy-first):**
- Use Piper with ElevenLabs/OpenRouter as fallback

**Development/testing:**
- Use Piper (fast, free, no API dependencies)

**Production app (quality matters):**
- Use ElevenLabs or OpenRouter depending on budget

**Multi-language support:**
- ElevenLabs: 29 languages, best quality
- Kokoro: 8 languages, good quality, cheap
- Piper: 20+ languages, acceptable quality, free

**VPS deployment (no local audio processing):**
- Use OpenRouter or ElevenLabs only (Piper requires Python + dependencies)

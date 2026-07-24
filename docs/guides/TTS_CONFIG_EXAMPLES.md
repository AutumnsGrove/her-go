# TTS Configuration Examples

Quick copy-paste configs for each TTS engine. Choose one and paste into your `config.yaml`.

---

## Example 1: Piper (Local, Free)

**Use this if:** You want privacy-first, zero-cost TTS with no API dependencies.

```yaml
voice:
  enabled: true
  stt:
    engine: "parakeet"
    base_url: "http://localhost:8765"
    model: "mlx-community/parakeet-tdt-0.6b-v2"
  tts:
    enabled: true
    engine: "piper"
    base_url: "http://localhost:8766"
    model: "en_GB-southern_english_female-low"
    voice_id: "en_GB-southern_english_female-low"
    speed: 1.0
    reply_mode: "voice"
    idle_unload_sec: 300
    pauses:
      paragraph_ms: 500
      line_ms: 250
      sentence_ms: 75
      comma_ms: 50
      semi_ms: 30
```

**Other Piper voices to try:**
- `en_US-amy-low` — American English, neutral
- `en_US-lessac-medium` — American English, clearer (uses more RAM)
- Browse all: https://rhasspy.github.io/piper-samples/

---

## Example 2: OpenRouter Kokoro (Remote, Cost Tracked)

**Use this if:** You want better quality than Piper with automatic cost tracking.

```yaml
voice:
  enabled: true
  stt:
    engine: "whisper"
    base_url: "https://openrouter.ai/api/v1"
    model: "openai/whisper-large-v3-turbo"
    fallback_model: "openai/whisper-large-v3"
  tts:
    enabled: true
    engine: "openrouter"
    base_url: "https://openrouter.ai/api/v1"
    model: "hexgrad/kokoro-82m"
    voice_id: "af_sarah"
    reply_mode: "voice"
```

**Note:** `api_key` is omitted — it automatically uses `openrouter.api_key` from the main config section.

**Other Kokoro voices to try:**
- `af_sky` — American female, upbeat
- `bf_lily` — British female, warm
- `am_adam` — American male, deep
- `bm_george` — British male, authoritative

Full list: https://openrouter.ai/hexgrad/kokoro-82m

**Cost:** ~$0.009 per day for 100 messages/day (150 chars each)

---

## Example 3: ElevenLabs (Remote, Premium)

**Use this if:** You want the highest quality TTS and don't mind the cost.

```yaml
voice:
  enabled: true
  stt:
    engine: "whisper"
    base_url: "https://openrouter.ai/api/v1"
    model: "openai/whisper-large-v3-turbo"
  tts:
    enabled: true
    engine: "elevenlabs"
    voice_id: "21m00Tcm4TlvDq8ikWAM"  # Rachel
    model: "eleven_flash_v2_5"
    api_key: "${ELEVENLABS_API_KEY}"
    reply_mode: "voice"
```

**Getting your voice_id:**
1. Go to https://elevenlabs.io/voice-library
2. Click a voice you like
3. Copy the `voiceId` from the URL:
   ```
   https://elevenlabs.io/voice-library/rachel/21m00Tcm4TlvDq8ikWAM
                                                   ^^^^^^^^^^^^^^^^^^
   ```

**Popular voices:**
- `21m00Tcm4TlvDq8ikWAM` — Rachel (calm, clear)
- `EXAVITQu4vr4xnSDxMaL` — Bella (warm, friendly)
- `jBpfuIE2acCO8z3wKNLl` — Gigi (young, energetic)

**Cost:** ~$4.50 per day for 100 messages/day (150 chars each)

---

## Example 4: OpenRouter with Piper Fallback

**Use this if:** You want OpenRouter quality but with a free backup when the API is down.

```yaml
voice:
  enabled: true
  stt:
    engine: "whisper"
    base_url: "https://openrouter.ai/api/v1"
    model: "openai/whisper-large-v3-turbo"
    fallback_model: "openai/whisper-large-v3"
  tts:
    enabled: true
    engine: "openrouter"
    base_url: "https://openrouter.ai/api/v1"
    model: "hexgrad/kokoro-82m"
    voice_id: "af_sarah"
    reply_mode: "voice"
    fallback:
      engine: "piper"
      base_url: "http://localhost:8766"
      model: "en_GB-southern_english_female-low"
      voice_id: "en_GB-southern_english_female-low"
      idle_unload_sec: 300
```

**How it works:**
- OpenRouter tries first (better quality, tracked cost)
- If OpenRouter fails for any reason → Piper kicks in
- Voice memos continue working seamlessly
- You see a warning in logs: `"primary TTS engine failed, falling back"`

---

## Example 5: ElevenLabs with Piper Fallback

**Use this if:** You want ElevenLabs quality but with a free backup when you hit quota limits.

```yaml
voice:
  enabled: true
  stt:
    engine: "parakeet"
    base_url: "http://localhost:8765"
    model: "mlx-community/parakeet-tdt-0.6b-v2"
  tts:
    enabled: true
    engine: "elevenlabs"
    voice_id: "21m00Tcm4TlvDq8ikWAM"
    model: "eleven_flash_v2_5"
    api_key: "${ELEVENLABS_API_KEY}"
    reply_mode: "voice"
    fallback:
      engine: "piper"
      base_url: "http://localhost:8766"
      model: "en_GB-southern_english_female-low"
      voice_id: "en_GB-southern_english_female-low"
      idle_unload_sec: 300
```

**Especially useful if:**
- You're on ElevenLabs' free tier (10K chars/month)
- You want voice to keep working even after you hit the limit
- Mid-month when you've exhausted quota but don't want to upgrade yet

---

## Mixed Configurations

You can mix and match STT and TTS engines independently:

### Local STT + Remote TTS
```yaml
stt:
  engine: "parakeet"  # local, free
  base_url: "http://localhost:8765"
  model: "mlx-community/parakeet-tdt-0.6b-v2"
tts:
  engine: "openrouter"  # remote, tracked cost
  base_url: "https://openrouter.ai/api/v1"
  model: "hexgrad/kokoro-82m"
  voice_id: "af_sarah"
```

### Remote STT + Local TTS
```yaml
stt:
  engine: "whisper"  # remote, tracked cost
  base_url: "https://openrouter.ai/api/v1"
  model: "openai/whisper-large-v3-turbo"
tts:
  engine: "piper"  # local, free
  base_url: "http://localhost:8766"
  model: "en_GB-southern_english_female-low"
  voice_id: "en_GB-southern_english_female-low"
```

---

## Switching Engines

1. **Edit config.yaml** — change the `engine` field and related settings
2. **Restart the bot:**
   ```bash
   systemctl --user restart her-go  # production
   # or
   ./her run  # development
   ```
3. **Test** — send a voice memo, bot should reply with the new voice

---

## Verifying Cost Tracking

After using OpenRouter TTS for a few turns:

```bash
her usage
```

You should see a line like:
```
tts    12    0    0    $0.0074
```

This confirms costs are being tracked properly!

---

## Troubleshooting

**"TTS synthesis failed: connection refused"**
- For Piper: sidecar isn't running. Check if `tts.enabled = true` so bot auto-starts it
- For OpenRouter/ElevenLabs: check your internet connection

**"TTS server returned 401"**
- Invalid API key. Check `openrouter.api_key` or `tts.api_key` in config.yaml

**"TTS server returned 429"** (ElevenLabs only)
- You hit your quota limit. Either upgrade plan or add Piper fallback

**Voice sounds robotic/choppy** (Piper)
- Try a higher-quality model: `en_US-lessac-medium` instead of `*-low`
- Increase `speed` to 1.1 or 1.2 for more natural pacing

**Costs seem wrong** (OpenRouter)
- Run `her usage` to see exact cost breakdown
- Cost is per input character, not per message

---

## More Help

Full TTS engine guide: `docs/guides/TTS_ENGINES.md`

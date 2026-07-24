# Configuration Guides

Detailed guides for configuring different aspects of the bot.

## Voice & Audio

- **[TTS_ENGINES.md](TTS_ENGINES.md)** — Complete guide to the three TTS engines (Piper, OpenRouter, ElevenLabs): when to use each, quality/cost tradeoffs, voice selection, troubleshooting
- **[TTS_CONFIG_EXAMPLES.md](TTS_CONFIG_EXAMPLES.md)** — Quick copy-paste configs for each TTS engine, including fallback configurations

## Quick Start: Switching TTS Engines

1. Read [TTS_ENGINES.md](TTS_ENGINES.md) to understand the tradeoffs
2. Pick an engine based on your priorities:
   - **Privacy + zero cost**: Piper (local)
   - **Good quality + tracked costs**: OpenRouter Kokoro
   - **Best quality**: ElevenLabs
3. Copy the config from [TTS_CONFIG_EXAMPLES.md](TTS_CONFIG_EXAMPLES.md)
4. Paste into your `config.yaml`
5. Restart: `systemctl --user restart her-go`

## Other Guides

*(More guides will be added here as the project grows)*

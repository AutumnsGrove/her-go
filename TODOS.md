# Project TODOs

## High Priority
- [ ] Set `telegram.owner_chat` in config.yaml (use /status to find chat ID) — scheduler won't deliver reminders without it
- [ ] Test /remind with various time expressions end-to-end

## Medium Priority
- [ ] v0.3: Fallback STT via CF Workers AI Whisper (for when away from Mac Mini)
- [ ] v0.3: Streaming LLM responses with live message editing
- [ ] v0.3: Webhook mode for Telegram (instead of long-polling)
- [ ] v0.3: Production deployment — Mac Mini + Cloudflare Tunnel
- [ ] v0.6 Scheduler Phase 2: recurring cron jobs, conditional tasks, run_prompt, mood/medication check-ins, damping, quiet hours

## Completed (v0.3)
- [x] Voice memo support (receive Ogg from Telegram, download via getFile)
- [x] Local STT via Parakeet (parakeet-mlx-fastapi sidecar, auto-started with `her run`)
- [x] Transcribed text enters normal pipeline (scrub → LLM → reply as text)
- [x] Store original audio file path in messages.voice_memo_path
- [x] VoiceConfig + STTConfig in config system
- [x] `her setup` installs ML dependencies (parakeet-mlx, parakeet-server, ffmpeg check)

## Completed (v0.2)
- [x] Scheduler phase 1: scheduled_tasks table, ticker loop, send_message task type
- [x] /remind <time> <message> — one-shot reminders
- [x] create_reminder agent tool
- [x] /schedule command — list upcoming reminders
- [x] go-naturaldate for proper time parsing

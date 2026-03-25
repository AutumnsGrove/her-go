# Project TODOs

## High Priority
- [ ] Set `telegram.owner_chat` in config.yaml (use /status to find chat ID) — scheduler won't deliver reminders without it

## Agent-Initiated Confirmations
- [x] `reply_confirm` tool — Yes/No inline keyboard buttons for destructive actions
- [x] `pending_confirmations` table — stores action payloads keyed by Telegram message ID
- [x] `handleConfirmCallback` — executes or cancels pending actions on button click
- [x] Supports: delete_expense, remove_fact, delete_schedule
- [x] 1-hour TTL on pending confirmations, double-click protection
- [ ] `reply_options` tool — multiple-choice inline keyboards (agent fills in options)

## Medium Priority
- [ ] Fallback STT via CF Workers AI Whisper (for when away from Mac Mini)
- [ ] Streaming LLM responses with live message editing
- [ ] Webhook mode for Telegram (instead of long-polling)
- [ ] Production deployment — Mac Mini + Cloudflare Tunnel

## Completed (v1.0 - Expenses)
- [x] OCR package (macos-vision-ocr + GLM-OCR fallback)
- [x] Pre-flight OCR on all photos (local, sub-200ms)
- [x] scan_receipt tool with line item extraction
- [x] query_expenses tool with period/category filtering
- [x] delete_expense and update_expense tools
- [x] expenses + expense_items tables
- [x] Agent prompt rules: financial data stays out of facts table

## Completed (v0.6)
- [x] Scheduler phase 2: cron jobs, mood/medication check-ins, quiet hours

## Completed (v0.3)
- [x] Voice memo support (receive Ogg from Telegram, download via getFile)
- [x] Local STT via Parakeet (parakeet-mlx-fastapi sidecar, auto-started with `her run`)
- [x] Transcribed text enters normal pipeline (scrub -> LLM -> reply as text)
- [x] Store original audio file path in messages.voice_memo_path
- [x] VoiceConfig + STTConfig in config system
- [x] `her setup` installs ML dependencies (parakeet-mlx, parakeet-server, ffmpeg check)

## Completed (v0.2)
- [x] Scheduler phase 1: scheduled_tasks table, ticker loop, send_message task type
- [x] /remind <time> <message> -- one-shot reminders
- [x] create_reminder agent tool
- [x] /schedule command -- list upcoming reminders
- [x] go-naturaldate for proper time parsing

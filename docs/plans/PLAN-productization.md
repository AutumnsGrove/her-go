# Productization Roadmap

## Context

her-go is a personal AI companion chatbot (inspired by the movie Her). Currently single-tenant and Telegram-only. This plan outlines the path to a hosted web product where users sign up and chat with their own instance of the bot.

**Target market:** People seeking persistent AI companionship (Replika/Nomi/Character.ai segment). Not a general-purpose assistant — a dedicated personality with memory, mood tracking, persona evolution, and dream cycles.

**Key differentiators:**
- 6-agent pipeline separating thinking from responding from remembering
- Memory cards with Zettelkasten-style linking and dream consolidation
- Persona that rewrites itself nightly based on accumulated reflections
- Mood tracking with emotional valence and daily rollups
- Privacy-first architecture (PII vault, local-first design)

---

## Phase 1: Cost Optimization (DONE)

See `PLAN-cost-optimization-web-chat.md`. Fast-path classifier, background agent batching, WebSocket adapter.

---

## Phase 2: Auth + Multi-Tenant (PLANNED)

### Auth Layer
- JWT-based authentication (access + refresh tokens)
- Separate auth SQLite database for user accounts
- HTTP endpoints: signup, login, refresh, logout
- WebSocket auth via JWT in upgrade query param
- New `auth/` package: store, jwt, handlers, middleware

### SQLite-per-User Storage
- `StoreManager` maps user_id → `memory.Store` instance
- Each user gets `data/users/{user_id}/her.db`
- Lazy loading, LRU cleanup for idle connections (30 min)
- Zero schema changes — Store interface works as-is
- `UserID` field added to `InboundMsg`, set from JWT claims

### Decisions Made
- SQLite-per-user over shared Postgres (avoids schema migration)
- SvelteKit for frontend (Autumn knows Svelte/TS)
- Hosted model (not self-hosted — too complex for onboarding)

---

## Phase 3: Web Product (PLANNED)

### SvelteKit Chat App
- WebSocket client with streaming token rendering
- Login/signup flow
- Chat message bubbles with markdown rendering
- Status and typing indicators
- Optional trace panel

### Deployment
- Fly.io with persistent volumes for SQLite files
- Single Go binary serves API + static frontend
- Scale-to-zero for idle users

### Pricing Model
- **Free tier:** 20 messages/day (acquisition)
- **Standard tier ($15-20/month):** 100-150 messages/day
- **BYOK tier ($5-10/month):** Unlimited, user provides OpenRouter key

---

## Open Questions

1. **Bot identity:** Does each user get independent persona evolution and dream cycles?
2. **Onboarding:** What does a new user's first conversation look like?
3. **Branding:** Is the product called "Mira"? Or is Mira the default persona?
4. **Model costs:** At ~$0.005/turn blended, $20/month supports ~130 messages/day. Viable but tight for heavy users.

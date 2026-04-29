# Plans Directory

Design documents and implementation plans for her-go features and infrastructure.

## Frontmatter Standard

All plan files use YAML frontmatter for metadata:

```yaml
---
title: "Human-readable plan name"
status: planning | ready | in-progress | complete | superseded | archived
created: YYYY-MM-DD
updated: YYYY-MM-DD
category: features | infrastructure | refactor | migration
priority: high | medium | low
related:
  - other-plan.md
  - ../docs/architecture.md
# Optional fields:
completed: YYYY-MM-DD      # For status: complete
superseded_by:             # For status: superseded
  - newer-plan.md
phases:                    # For multi-phase plans
  - phase-name
---
```

### Status Values

- **`planning`** — Design phase, not yet started
- **`ready`** — Design complete, ready for implementation
- **`in-progress`** — Actively being built
- **`complete`** — Fully implemented and shipped
- **`superseded`** — Replaced by another plan (see `superseded_by`)
- **`archived`** — No longer relevant, kept for reference

### Categories

- **`features`** — New user-facing functionality
- **`infrastructure`** — Internal systems (testing, sandboxing, agent improvements)
- **`refactor`** — Code quality improvements, consolidation
- **`migration`** — Data or API migrations

## Quick Reference

### Current Plans by Status

**High Priority (Ready/In Progress):**
- [Test Infrastructure](TEST_PLAN.md) — in-progress
- [Coding Agent](PLAN-coding-agent.md) — planning
- [Always-On Infrastructure](PLAN-always-on-infra.md) — planning

**Ready for Implementation:**
- [Calendar Bridge](PLAN-calendar-bridge.md)
- [Shift Tracking](PLAN-shifts.md)
- [Scheduler One-Offs](PLAN-scheduler-oneoffs.md)
- [Mood Tracking Redesign](PLAN-mood-tracking-redesign.md)
- [Apple Reminders Bridge](PLAN-reminders-bridge.md)

**Completed:**
- [Sim Calendar Extensions](PLAN-sim-calendar.md) ✓

**Superseded:**
- [Calendar Integration (Monolithic)](PLAN-calendar-shifts.md) — split into 4 focused plans

**Backlog (Planning):**
- [Classifier Improvements](PLAN-classifier-improvements.md)
- [Embedding Similarity Consolidation](PLAN-similarity-consolidation.md)
- [Zettelkasten Memory](PLAN-zettelkasten-memory.md)

## Query Plans by Status

```bash
# List all plans with status
grep -h "^status:" docs/plans/*.md | sort | uniq -c

# Find all ready-to-implement plans
grep -l "status: ready" docs/plans/*.md

# List high-priority plans
grep -l "priority: high" docs/plans/*.md

# Show all incomplete plans (planning, ready, in-progress)
grep -L "status: complete\|status: superseded\|status: archived" docs/plans/*.md
```

## Related Documentation

- [Architecture Overview](../ARCHITECTURE.md) — Data flow and model calls
- [Skills Architecture](../skills-architecture.md) — Archived skills system design
- [Migration: Postgres](../migration-postgres.md) — SQLite → Postgres considerations

# Sim baselines (Nova migration)

Structural digests of sim runs, captured so a later run can be compared against
them without diffing prose. Produced by `her digest <report.md> --out <dir>` and
compared with:

```bash
her digest --compare docs/plans/baselines/<baseline>.json --compare <new-digest>.json
```

The comparison applies the tolerances from
[PLAN-nova-migration.md](../PLAN-nova-migration.md) §4.3 and exits non-zero on a
failed check, so it can gate CI or a deploy. Digests contain no prompts, no
replies and no secrets — only turn counts, tool names, costs, token counts,
provider names and memory/mood counts.

## What was captured, and on what stack

| Run | Suites | Stack |
|---|---|---|
| `base1` | smoke: schedule-crud, worker-agent-test, vision-test, compaction-quick, mood-dedup-test | Go 1.27.3, MiMo 2.6 (pro chat / flash agents), OpenRouter `pplx-embed-v1-0.6b` @ 1024 dims, legacy loop, legacy transport |
| `base2` | same five, second run | same |
| `base1core` | core additions: core-loop-and-dream, memory-a-thon, card-lifecycle, introspection-test, dream-consolidation | same |

Two runs of the smoke set exist because LLM output is not deterministic: the
spread between `base1` and `base2` is the noise floor, and it is what makes the
"within ±25% cost / ±30% memories" bands meaningful rather than invented. The
core set was captured once, for wall-clock and budget reasons — recorded as a
deviation in [NOVA-MIGRATION-LOG.md](../NOVA-MIGRATION-LOG.md).

The baselines are the *pre-migration* stack: they include the Go bump, the
OpenRouter embeddings and the MiMo 2.6 model bump, so any difference after them
is attributable to the Nova seams rather than to a model change.

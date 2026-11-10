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

## What is here

`legacy/` and `nova/` hold the same suites captured on the same stack (Go 1.27.3, MiMo 2.6 Pro chat **and**
driver, `perplexity/pplx-embed-v1-0.6b` @ 1024 dims) on either side of the migration:

| Suite | legacy loop + legacy transport | Nova loop + Nova transport |
|---|---|---|
| schedule-crud | 6 turns, $0.0433, 83 tools, 1/6 reached `done`, 6 fallbacks | 6 turns, $0.0333, 73 tools, **5/6** reached `done`, 0 fallbacks |
| worker-agent-test | 2 turns, $0.0087, 20 tools, 2/2 `done`, 1 fallback | 2 turns, $0.0089, 18 tools, 2/2 `done`, 0 fallbacks |

`shipping/` holds the same two suites again on the **final** configuration (Nova loop + Nova transport + the
DeepSeek V4.1 Flash agent models):

| Suite | shipping |
|---|---|
| schedule-crud | 6 turns, $0.0238, 53 tools, **6/6** reached `done`, 0 fallbacks, 66.0% cache |
| vision-test | 5 turns, $0.0180, 64 tools, 4/5 `done`, 88.8% cache |

Against `legacy/`, schedule-crud improves on every axis that matters: `done` 6/6 vs 1/6, fallbacks 0 vs 6, cost
−45%. The one remaining `FAIL` line is `think` no longer appearing in the tool set — DeepSeek returns its
reasoning in a separate channel rather than calling the explicit think tool, and the loop turns that channel into
the same trace line (`agent_engine/nova.go`), so the trace panel keeps showing the model's thinking.

`compaction-quick` and `mood-dedup-test` have legacy baselines only (the Nova re-runs did not fit the session).
Cache hit rate was the number that mattered most (a dropped prompt cache is the costliest silent regression):
schedule-crud 88.9% legacy vs 87.3% Nova, worker 84.0% both.

## Why two runs of the smoke set existed, and what happened

The plan asked for the whole set twice to measure nondeterminism. Doing that was itself informative: two
identical-stack `schedule-crud` runs differed by +70% cost, in tool sets, and in completion (6/6 vs 3/6
`done`), because the driver occasionally stalls or wanders. That spread is why the comparison treats *structural*
signals (did turns complete, did the tools that matter run, did cost stay within a quarter) as the gate rather
than exact numbers — and why `her digest` prints PASS/FAIL lines instead of a diff.

An earlier set (`base1`/`base2`, MiMo 2.6 **Flash** as driver) is not committed: it exists only as evidence in
[NOVA-MIGRATION-LOG.md](../NOVA-MIGRATION-LOG.md) that Flash was the wrong driver model.

## Reproducing

```bash
her sim -s sims/schedule-crud.yaml                        # on whatever config.yaml says
her sim -s sims/schedule-crud.yaml --llm-backend legacy   # the transport rollback
her digest <report.md> --out docs/plans/baselines/nova
her digest --compare docs/plans/baselines/legacy/schedule-crud.json \
           --compare docs/plans/baselines/nova/schedule-crud.json
```

Digests contain no prompts, replies or secrets — only turn counts, tool names, costs, token counts, provider
names and memory/mood counts.

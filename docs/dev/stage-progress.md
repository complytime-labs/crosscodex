# Stage task progress

How `run status` knows how many tasks a stage has finished. User-facing meaning of the counts is in the README ("Seeing progress"). Table: `job_stage_progress` (migration `004_stage_progress`, see [migrations.md](migrations.md)). Metrics: [telemetry.md](telemetry.md).

## Flow

```text
collector (internal/analysis)  --events-->  stageProgress  --deltas-->  ProgressStore  -->  job_stage_progress
 success / retry / final failure            in-memory sum    every flush interval          one row per (tenant, job, stage)
                                                             and once at stage end                      |
                                          GetJob (internal/pipeline) --JobStage.progress--> gateway --> crosscodex run status
```

1. The engine builds one `stageProgress` per stage after it has split the stage's tasks into cache hits, other pre-built results and tasks to dispatch (`Start(total, preBuilt, cacheHits)`). `total` is dispatched plus pre-built tasks. Pre-built results, which include cache hits, count as succeeded but never as a worker success, so they do not set `last_success_at`.
2. The collector reports each event to the observer as it handles it: `TaskSucceeded`, `TaskRetried` and `TaskFailed` (a task's final failure after retries, including the missing-original-task and failed-redispatch paths; without those `done` would never reach `total`).
3. A ticker goroutine writes the pending delta every `analysis.engine.progress_flush_interval` (default 10s) while there are new events. `Finish` stops it and writes once more, on a context detached from the stage's cancellation and bounded to 30s, so a cancelled or failed stage still records its final counts. Both completion paths call `Finish` before the stage's output is completed; a deferred `Finish` covers every other exit.
4. `PGStore.AddStageProgress` upserts the row inside a tenant-scoped transaction. `GetJob` reads the rows into `JobStage.progress`; `ListJobs` does not read them.

## Why additive deltas

- **Resume.** A resumed stage runs again with its own `total` and counts. Adding to the stored row keeps what the earlier run did, and a stage that was interrupted and resumed shows the work of both runs. The price is that `done` can exceed `total`: the README says so.
- **No read-modify-write.** The writer sends only what happened since its last write and the database adds it. There is no read of the stored value, so no lost update, and `last_success_at` takes `GREATEST` of the stored and delta times. `total` is the exception: it replaces the stored value with the latest run's task count.
- **Failed writes lose nothing.** A failed write puts its delta back into the pending one, merged with events that arrived meanwhile, so the next write carries both.

## Accepted limit: lost commit responses

The store has no idempotency key. If a write commits but the response is lost (the connection drops after `COMMIT`), the engine sees a failure, re-queues the delta and the next write adds it again. The counts of that one delta are then double. The figures are operational, not evidence (nothing attests them and no result depends on them), and a stage's results are unaffected, so the limit was accepted over adding a per-write key and a table to deduplicate. Do not use these counts for billing or verification.

## Failure handling

- A failed write while the stage runs never fails the stage. It is logged at warn level and counted (`analysis.progress.flushes.total`, `outcome=failed`); the counts lag until a later write succeeds.
- A failed final write leaves the counts of a completed stage behind. The stage sets `Output.Metadata["progress_stale"] = "true"` (`MetadataProgressStale`). That metadata reaches only the NATS completion event (`internal/analysis/reporter.go`, the `metadata` field, which feeds the graph role's MATERIALIZE stream). `DBStageReporter` does not write it to Postgres, so `GetJob` and `run status` cannot show it: an operator learns of a stale stage from the log line and the failed-flush counter, not from the CLI.
- `AddStageProgress` writes only for a job of the context's tenant (`WHERE EXISTS` under row-level security) and returns `ErrNotFound` otherwise. A plain upsert would let the foreign key to `jobs` accept another tenant's job ID, which leaks that the job exists through the error and creates a row the other tenant's purge cannot see.

## Where it does not run

- **Embedded mode** builds no analysis engine (`localPipelineBackend` records jobs and runs nothing), so it writes no progress rows itself. Its `GetJob` still shows the rows an engine already wrote to the same database.
- Without a `ProgressStore` (`WithProgressStore` not given) the engine builds no observer and the collector's progress hook is nil.
- Jobs that ran before migration `004` have no rows, and stay without.

## Changing it

- A new count is a new column in `004` (or a later migration), a field of `ProgressDelta` and `pb.StageProgress`, a term in the `SET` list of `AddStageProgress`, and a field of `stageProgressJSON` and `writeProgressLines` in `cmd/crosscodex`. Keep count columns `NOT NULL DEFAULT 0` with a `>= 0` check, and keep `tasks_done = tasks_succeeded + tasks_failed` (a table constraint).
- Keep the observer's methods non-blocking: they run on the collector's result and retry goroutines.

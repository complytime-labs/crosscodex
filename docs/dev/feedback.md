# Mapping Feedback

Maintainer guide to `internal/feedback`: reviewers' votes on a job's mappings,
the review queue, and determinations that a mapping is valid or invalid
(issue #230). The design record, with the rejected alternatives, is
[feedback-cleaning-results-spec.md](feedback-cleaning-results-spec.md); the
operator view is the README's "Mapping Feedback" section. Feedback reuses
[cleaning](cleaning.md)'s graph operations and verdict read and nothing else.

## Mapping identity

A mapping is one synthesized `vote_summaries` row of a **completed** job.
`mapping_id` is `DerivedID("semantic-match", job, source, target)`, the ID of
the SEMANTIC_MATCH edge the job wrote. It names the **verdict** (one job's
judgement of one control pair, `cleaning.VerdictKey`) and never moves to
another edge: a restore creates edges under new IDs and `mapping_id` stays
the same. To reach graph state, resolve the verdict relationally (the job's
mappings, so every request carries `job_id`), then read its live edges in both
stages with `cleaning.Engine.VerdictEdges`. Never follow edge IDs.

Mapping reads (`Store.JobMappings`) run with `tenant.WithoutUser`: the jobs
policy would otherwise confine them to the job's creator, and reviewers act on
other users' jobs. The reviewer or admin role gate is in `Service`, so the read
is tenant-wide only for those callers.

## Tables (migration `003_mapping_feedback`)

| Table | Rows | Grants for `app_user` | Notes |
|---|---|---|---|
| `mapping_votes` | one per vote, append-only | `SELECT, INSERT` | a reviewer's latest vote on a mapping is counted (`LatestVotes`) |
| `mapping_determinations` | one per determination, append-only | `SELECT, INSERT` | `outcome` is `valid`, `invalid` or `stale`; `decided_by = 'reproject'` exactly for `stale`; stores the canonical request and the signed link |
| `mapping_reviews` | one mutable row per decided mapping | `SELECT, INSERT, UPDATE` | status, latest determination and its time, edge IDs, `graph_applied`; no `DELETE` |

`refuse_change` triggers keep the two append-only tables append-only. RLS keys
off `app.current_tenant` on all three. None is purged by retention: they are
audit records, like `edge_findings`. The migration also adds `stale` to
`edge_findings.status` and `edge_finding_decisions.verdict`, and the view
`mapping_cleaning_states` (`security_invoker`, so RLS applies), which results
and feedback both read to learn a mapping's latest cleaning or determination
change. `PGStore` refuses a vote or determination at write time unless the
mapping belongs to a completed job visible to the caller and the row passes
`Vote.Validate` / `Determination.Validate`; a determination also needs an
active registered reviewer and the review state its caller read, checked under
a row lock (`ErrReviewChanged`).

## Votes and the queue

`SubmitVote` validates at the handler (`Vote.Validate`): `vote_type` known,
`suggested_type` required with and only with `REFINE`, `rationale` required
except for `AGREE` and at most 4096 bytes, `evidence` at most 32 entries with
keys up to 128 and values up to 1024 bytes, strings valid UTF-8 without control
characters. The voter is the authenticated subject, never a request field.

`GetReviewQueue` lists the job's synthesized mappings (viable or not) in
(source, target) order, with the vote aggregation (`Aggregate`). The page token
is the base64url JSON of the last item's (source, target), so it is a keyset
cursor, not an offset. Defaults: page size 50, at most 1000; larger or negative
is `InvalidArgument`. A token that does not decode is `InvalidArgument`
(`FuzzDecodePageToken`). Known limit: every page reads and aggregates the whole
job (the keyset position and filter are applied in memory, because
`total_count` needs every match), so one page costs O(mappings in the job) and
paging through a job costs O(N²/page size). Push the position and filter into
the store query, or cache the aggregation per job, if jobs grow large enough
for this to show. The thresholds are `feedback.min_agreement` and
`feedback.low_confidence`; see the README for the filters and their meaning.

## Determinations

`RecordDetermination` needs the reviewer role and an active registered
reviewer (the same sentinels as cleaning decisions: `ErrReviewerNotRegistered`,
`ErrReviewerDeleted`). `final_type` may be unset or the mapping's own type;
anything else is refused with an instruction to record `is_valid=false` and
re-run the analysis. Transitions on the mapping's review:

| Review now | `is_valid=false` | `is_valid=true` |
|---|---|---|
| none, `valid`, `stale` | retire the verdict's live edges; refused with `ErrNoLiveEdge` when it has none (cleaning retired it, or its job's edges are gone) | record the review `valid`; no graph change |
| `invalid` | refused with `ErrAlreadyInvalid`, naming the determination | restore the retired edges; refused with `ErrEdgesGone` when a rebuild removed them (run `crosscodexd admin cleaning --reproject` first) |

A valid determination on a mapping that is already valid (including one
restored earlier) stores no edge IDs, so results stop showing that mapping as
`restored`. This is accepted: the reviewer's latest word is that the mapping
stands, and the restore stays in `mapping_determinations`.

Order of work: apply outstanding projections (below), read the review, compute
the edge change, build the canonical request (`determinationRequest`: tenant,
job, mapping, verdict, validity, final type, rationale, sorted evidence,
reviewer, the determination it replaces, the vote aggregation, the edge IDs and
`decided_at`), hash it (SHA-256 is the determination ID), sign it as a
`feedback.determination` in-toto link (material
`crosscodex:mapping-determination/<id>`, product
`crosscodex:mapping/<mapping_id>`), record it with `graph_applied = false`, then
change the graph through `cleaning.RetireEdges` / `RestoreEdges` timed by the
determination, then `MarkProjected`. The request snapshot includes the
aggregation so the record shows what reviewers had said. Signing needs
`attestation.private_key_path` and `attestation.public_key_path` whatever
`attestation.enabled` says (`ErrSigningDisabled`).

## Projection and retry

The relational rows are authoritative; the graph is a projection of them.
`graph_applied` is false from the moment a determination is recorded until its
edge changes are written. If the graph write stops, the determination stands,
the call fails saying so, and the next `RecordDetermination` applies every
outstanding one first; it is refused (nothing recorded, `ErrUnprojected`) while
that cannot finish, with the graph error in the daemon log. Every write is
checked against the determination read (`MarkProjected` and `MarkReprojected`
take the expected determination), so a concurrent determination is never
overwritten. `feedback.reviews.unprojected` shows the count.

## Re-projection (`Service.Reproject`)

Called by `crosscodexd admin cleaning --reproject` after cleaning's own
`Engine.Reproject`; see "Re-projection after a rebuild" in
[cleaning.md](cleaning.md) for the shared rules. No LLM call. It is refused
without `WithDeterminations` (`ErrDeterminationsUnavailable`), while any review
is unprojected (`ErrUnprojected`, with the fixed text naming the next step),
and without a signer unless `dryRun` (`ErrSigningDisabled`), all before any
write. It considers each `invalid` review and each `valid` review that holds
restored edge IDs, in mapping ID order, and reads the verdict's live edges
(none when its job is no longer completed: retention purged it), which stored
edge IDs the graph still holds, and whether the original edge (ID equal to
`mapping_id`) exists.

| Condition | Action | Writes |
|---|---|---|
| invalid, with a live edge (rebuilt, or live again) | `retire` | the edges join the review's edge IDs and are superseded at the determination's time, or at an edge's `valid_from` when that is later |
| live edges that are not the stored IDs, or stored IDs gone while others remain | `relink` | the review's edge IDs become the live (or remaining) edges |
| no live edge, no stored edge ID, no original edge, and no edge of the verdict at all (`cleaning.PresentVerdicts`) | `stale` | a signed `stale` determination (`decided_by = reproject`, `feedback.reproject` link) naming the missing verdict; review status `stale` |
| anything else | `in_sync` | nothing |

The "present" rule: a verdict counts as present if it has a live edge, any
stored edge ID exists, the original edge exists, or the graph holds any
`SEMANTIC_MATCH` edge of the verdict (its source, target and `job_id`), live
or superseded, either stage, under any edge ID (`cleaning.PresentVerdicts`,
shared with cleaning's re-projection). The last test covers the two engines
governing one verdict independently: when a reviewer restored a mapping and
cleaning later retired it, cleaning's re-projection (which the CLI runs first)
retires the rebuilt edge under the rebuild's ID kind, which the review never
stored, and the review must not be marked stale for it. It also keeps a
review whose job retention purged `in_sync` while the graph still holds its
verdict's edges; such a review cannot read the verdict's live edges, so they
are not retired either. The cost is that a verdict whose live and stored
edges are gone but whose original edge, or any other edge of it, survived
stays `in_sync`. A `stale` review takes a new determination as the
"none" row of the transition table does. A second run reports every review
`in_sync`. A failure part-way leaves earlier reviews
re-projected; run again to finish. The CLI runs both engines as dry runs first
and writes nothing when either refuses (see the README).

## Errors

Sentinels map to Connect codes in `serviceError` (`service.go`, the `mapped`
list, first match wins): `cleaning.ErrTenantMismatch` to `PermissionDenied`,
`ErrInvalid` to `InvalidArgument`, `ErrJobNotFound`, `ErrMappingNotFound` and
`cleaning.ErrVerdictJobNotFound` to `NotFound`, `ErrReviewChanged` to
`Aborted`, and `ErrDeterminationsUnavailable`, `ErrUnprojected`,
`ErrAlreadyInvalid`, `ErrNoLiveEdge`, `ErrEdgesGone`, `ErrSigningDisabled` and
the reviewer sentinels to `FailedPrecondition`. A job that is not completed is
`FailedPrecondition` too (`JobNotCompletedError`).
`ErrDeterminationsUnavailable` is `FailedPrecondition`, not `Unavailable`,
because the gateway withholds `Unavailable` messages and the caller must read
which daemon to use. Withholding (CWE-209): a mapped
sentinel keeps its whole message only when every leaf of the error tree is a
mapped sentinel (`onlySentinels`); otherwise a leaf may be a driver error and
only the sentinel's own text goes out. Unmapped errors pass to the
gateway's `publicError`, which withholds them.

## Shapes

| Shape | Votes, queue | Determinations |
|---|---|---|
| role `all` | in process (`newFeedbackService`) | in process, `WithDeterminations` over the role's cleaning engine and signer |
| `gateway` + `graph` | gateway forwards to the graph role's listener | same, on the graph role |
| embedded daemon | in process | refused with `FailedPrecondition`: "mapping determinations need the crosscodexd daemon (role all, or gateway with a graph role)" |

The embedded daemon's client certificate has roles `admin` and `reviewer`, so
its one local user can vote. Wiring is in [service-runtime.md](service-runtime.md).

## Telemetry

`WithTelemetry` (service) and `WithStoreTelemetry` (store) take the daemon's
providers; neither has a config field. Spans `feedback.<Method>` (attributes
`tenant.id`, `feedback.operation`) and `feedback.store.<Op>`. Metrics:
`feedback.votes.total`, `feedback.operation.duration_ms`,
`feedback.reviews.unprojected`, `feedback.store.operations.total`,
`feedback.store.operation.duration_ms`; see
[telemetry.md](telemetry.md#registered-metrics). Re-projection also adds the
`reproject` operation to `cleaning.operation.duration_ms` and a
`cleaning.Reproject` span.

## Tests

- Unit and property specs: `internal/feedback` (`service_bdd_test.go`,
  `determine_bdd_test.go`, `reproject_bdd_test.go`, `store_bdd_test.go`,
  `service_property_test.go`), `internal/gateway`, `internal/graph`,
  `internal/results`, `pkg/cleaning` and `cmd/crosscodexd`.
- `task test:integration:feedback`: the store contract, RLS, the append-only
  tables and the `mapping_cleaning_states` view against PostgreSQL
  (`store_integration_bdd_test.go`).
- Fuzz targets (human-run only; agents never run them): `FuzzVoteValidate`,
  `FuzzDeterminationValidate` and `FuzzDecodePageToken` in
  `internal/feedback/feedback_fuzz_test.go`.

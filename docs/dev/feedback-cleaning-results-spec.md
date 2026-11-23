# Spec: Mapping Feedback, Cleaning State in Results, Cleaning Re-projection

Issues: #230 (remaining gaps), #291, #289. Status: accepted for implementation
(2026-11-22). This is the design record; how the finished code works is in
[feedback.md](feedback.md) and [cleaning.md](cleaning.md).

## Requirements and how they are verified

| # | Requirement | Verified by |
|---|---|---|
| R1 | `SubmitVote`, `GetReviewQueue` and a new `RecordDetermination` work in role `all` and in a split deployment, and refuse with a message naming the shape that serves them where they cannot work (determinations in the embedded daemon) | `internal/feedback` and `internal/gateway` specs; `cmd/crosscodexd` and `cmd/crosscodex` wiring specs |
| R2 | An invalid determination retires the mapping's live edges, and a later valid one restores them, only through `pkg/cleaning.RetireEdges` / `RestoreEdges` | `internal/feedback` specs over `memdriver` |
| R3 | AdminService (retention, legal holds) is served by the standalone gateway and the embedded daemon, not only role `all` | wiring specs; `internal/admin` specs for the no-purge mode |
| R4 | `crosscodex results` shows each mapping's cleaning state, tenant-scoped, without changing stored results | `internal/results` specs and PostgreSQL integration spec (RLS) |
| R5 | One rule says what a `mapping_id` names after a retire or restore, used by results and feedback | this spec, README "Upgrading", feedback specs |
| R6 | `crosscodexd admin cleaning --reproject` replays cleaning and feedback state into a rebuilt graph, makes no LLM call, refuses while projections are outstanding, and marks findings whose verdicts are gone as `stale` with a signed reason | `pkg/cleaning` and `internal/feedback` specs; `cmd/crosscodexd` specs |

## The mapping identity rule (R5, shared by #230 and #291)

A `mapping_id` is `DerivedID("semantic-match", job, source, target)`. It names
a **verdict**: one job's judgement of one control pair in its stored direction
(`cleaning.VerdictKey`). It is a stable key and never moves to another edge.

- To reach a mapping's graph state, resolve `mapping_id` to its verdict
  relationally (the job's mappings, so requests carry `job_id`), then read the
  verdict's live edges in both stages the way cleaning does
  (`cleaning.Engine.VerdictEdges`, built on `crosswalk.LoadPair`). Never follow
  edge IDs.
- A mapping's cleaning state is the latest effective change recorded for its
  verdict in `edge_findings` (`retired`, `restored`, `stale`) or
  `mapping_reviews` (a determination). One SQL view,
  `mapping_cleaning_states`, defines it; results and feedback both read it.

**Decision.** Stable key resolved by verdict. *Rejected:* a lookup from any
edge ID to its live successor by following `Supersedes`. The standalone gateway
serves `results` with no graph access, a verdict owns two edges (analyzer and
synthesized stage), restore chains branch per stage, and the graph-rebuild
materializer writes SEMANTIC_MATCH edges under another ID kind, so "the" live
successor of an edge ID is neither reachable nor well defined there.

## #230 Mapping feedback

### Behaviour

- **SubmitVote** (reviewer): `job_id`, `mapping_id`, `vote_type`
  (`AGREE`, `DISAGREE`, `REFINE`, `ESCALATE`), `suggested_type` (required with
  and only with `REFINE`, never `UNSPECIFIED`), `rationale` (required except for
  `AGREE`, at most 4096 bytes), `evidence` (at most 32 entries, keys at most 128
  bytes, values at most 1024 bytes). All strings must be valid UTF-8 without
  control characters. The voter is the authenticated subject. Votes are
  append-only; a reviewer's latest vote on a mapping is the one counted.
- **Aggregation**: counts of latest votes per type; `agreement_score` = agree /
  counted votes (0 with none); `consensus_type` = the most suggested refine
  type when refine votes outnumber every other type (ties go to the mapping's
  own type), else the mapping's type; `needs_escalation` = any escalate vote,
  or at least one vote and `agreement_score < feedback.min_agreement`.
- **GetReviewQueue** (reviewer or admin; it was admin only): a new required
  `job_id`. Items are the job's synthesized mappings, viable or not, ordered by
  (source, target), with their aggregation. Filters: `LOW_CONFIDENCE`
  (confidence below `feedback.low_confidence`), `HIGH_DISAGREEMENT` (votes and
  agreement below `feedback.min_agreement`), `ESCALATED` (an escalate vote),
  `UNVOTED` (no vote). `UNSPECIFIED` lists mappings matching any of the first
  three; each item's `reason` is the first match in the order escalated, high
  disagreement, low confidence. Pages default to 50, at most 1000 (larger or
  negative is `InvalidArgument`), with an opaque keyset token.
- **RecordDetermination** (reviewer, active registered reviewer as for
  cleaning decisions): new GatewayService RPC. `job_id`, `mapping_id`,
  `is_valid`, `determination_rationale` (required), `evidence`, `final_type`
  (`UNSPECIFIED` or the mapping's own type; changing a type is refused with an
  instruction to record `is_valid=false` and re-run the analysis).
  Transitions on the mapping's review (`mapping_reviews`):

  | Current | `is_valid=false` | `is_valid=true` |
  |---|---|---|
  | none, `valid`, `stale` | retire the verdict's live edges; refused when it has none (retired by cleaning or gone) | record; no graph change |
  | `invalid` | refused: already invalid, name the determination | restore the retired edges |

  The determination is signed (`feedback.determination` link; material
  `crosscodex:mapping-determination/<id>` = SHA-256 of the canonical request,
  product `crosscodex:mapping/<mapping_id>`), recorded, then projected; the
  graph change is timed by the determination. Outstanding projections are
  finished before a new determination is recorded. The request snapshot
  includes the vote aggregation, so the record shows what reviewers had said.

**Decision.** Determinations are an explicit reviewer action. *Rejected:*
automatic determination once votes cross a threshold, which lets the N-th
vote mutate the graph implicitly and is harder to audit.

**Decision.** Feedback keeps its own tables and reuses only cleaning's graph
operations and verdict read, as the #272 design note asks. *Rejected:*
recording determinations as cleaning findings of a new signal; it would bend
the finding lifecycle (a signal no pass detects) to save code.

### Storage (migration `003_mapping_feedback`)

- `mapping_votes`, `mapping_determinations`: append-only (`refuse_change`
  triggers, `app_user` `SELECT, INSERT`), RLS on `app.current_tenant`.
  `mapping_determinations.outcome` is `valid`, `invalid` or `stale`
  (`decided_by = 'reproject'` exactly for `stale`).
- `mapping_reviews`: one mutable row per mapping (status, latest determination,
  its time, edge IDs, `graph_applied`); `SELECT, INSERT, UPDATE`, no `DELETE`.
- None is purged by retention (audit records, like `edge_findings`).
- `edge_findings.status` and `edge_finding_decisions.verdict` gain `stale`.
- View `mapping_cleaning_states` (`security_invoker`, so RLS applies).

### Shapes

| Shape | Votes, queue | Determinations |
|---|---|---|
| role `all` | in process | in process |
| split (`gateway` + `graph`) | gateway forwards to the graph role's listener | same |
| embedded daemon | in process | refused `FailedPrecondition`: needs the crosscodexd daemon (role all, or gateway with a graph role) |

### Configuration

`feedback.min_agreement` (default 0.5) and `feedback.low_confidence`
(default 0.5), both in [0, 1]. Global only: a review threshold is a property
of the deployment's review process, not of a tenant's data.

## AdminService in every gateway shape (#230)

- Standalone gateway: builds the same retention wiring as role `all`
  (`buildRetentionWiring`) and mounts AdminService. `database.purge_dsn`
  enables purges and catalog retire there as in role `all`; leaving it unset
  keeps purge credentials off the internet-facing process, and purges then
  fail closed.
- Embedded daemon: mounts AdminService with holds, stats and dry-run scans. It
  has no NATS audit stream and no purge role, so a non-dry-run scan or policy
  apply is refused with `FailedPrecondition` naming the daemon that can purge
  (new `admin.WithoutPurge(reason)`). Stats report no stream drift because the
  embedded daemon runs no audit streams.

## #291 Cleaning state in results

`ControlMapping.cleaning` (`MappingCleaning`: `state` `retired`, `restored` or
`stale`; `source` `finding` or `determination`; `ref_id`; `changed_at`;
`graph_applied`) is set from `mapping_cleaning_states` in the same
tenant-scoped transaction as the page. Unset means no cleaning or feedback
change applies. `results debug` gains a `Cleaning:` line per mapping (synthesized stage only),
JSON a `cleaning` object, CSV a trailing `cleaning_state` column (README
"Upgrading").

## #289 Re-projection

`crosscodexd admin cleaning --reproject (--tenant <id> | --all-tenants)
[--dry-run]`. On-host, opens the database and graph, no LLM client. Refuses
when any finding or determination is unprojected (run `crosscodex admin
cleaning run --resume` first) and, unless `--dry-run`, without a signer.

For each finding in `retired` or `restored`, and each review in `invalid` or
`valid` after a restore, using the latest retire/restore decision:

- **Verdicts present** (some target verdict has a live edge, or a stored edge
  ID still exists): retired → retire the live edges at the decision time, or at
  an edge's `valid_from` when that is later (a rebuilt edge did not exist
  earlier); restored → store the verdicts' live edge IDs. Already consistent →
  nothing.
- **Verdicts gone** (no live edge and no stored edge ID in the graph, and no
  `SEMANTIC_MATCH` edge of the verdicts at all, live or superseded, under any
  edge ID: the graph was rebuilt from new jobs): mark `stale` with a signed
  decision whose reason names the missing verdicts. A verdict whose only edges
  the other engine retired is present, and nothing is changed. `stale` takes no decision; a later pass that
  detects the finding again raises it as `new`.

**Decision.** New jobs → mark stale. *Rejected:* mapping old decisions onto
the new jobs' verdicts for the same control pair; it needs an equivalence rule
between different jobs' judgements, which the data cannot decide.

## Personas

- **Auditor:** every determination and stale marking is a signed, append-only
  row naming who, when, why and what the votes said; results show which
  finding or determination changed a mapping, by ID; `mapping_id` keeps
  naming the same verdict.
- **Security engineer:** every new input is bounded and validated at the
  handler; reviewer role and active-reviewer checks; tenant from the
  authenticated context only; RLS on all new tables and the view; errors from
  stores pass through `publicError`; the gateway's purge credentials stay
  opt-in.
- **Developer:** proto comments state required fields, limits and refusals;
  additive proto changes only; README "Upgrading" for the CSV column, the
  queue's `job_id` and the reviewer role on the queue.
- **Operator:** feedback calls no LLM and does a bounded read (one job, one
  catalog pair); `--reproject --dry-run` previews; tuning is in
  `pkg/config` `feedback`; telemetry for every new public method.

## Not included

`GetVotes`, `GetUserVotes`, `EscalateMapping` (escalation is a vote type),
`PipelineService.GetJobTrace`, and reviving a `stale` finding.

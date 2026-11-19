# Edge Cleaning

This document describes how edge cleaning works: the signals, the finding
lifecycle, the tokens that bind a write to what was read, the write order
that makes graph changes resumable, and the audit record. For operator and
user instructions (CLI, config, roles) see "Edge Cleaning" in the top-level
`README.md`.

Code: `pkg/cleaning` (detection, planning, store, engine, graph operations),
`internal/cleaning` (the `EdgeCleaningService` handler),
`cmd/crosscodex/admin_cleaning.go` (CLI), `api/proto/crosscodex/v1/cleaning.proto`
(contract) and the `edge_cleaning_runs` / `edge_findings` /
`edge_finding_decisions` tables in
`pkg/db/migrations/001_initial_schema.up.sql`.

## Overview

Mapping jobs accumulate `SEMANTIC_MATCH` verdicts in the graph. Cleaning reads
them, raises findings where they look wrong or redundant, and retires or
restores them under the tenant's policy or a reviewer's decision.

- A **verdict** is one job's judgement of one control pair in one stored
  direction: `VerdictKey{JobID, SourceID, TargetID}`, written
  `<job>:<source>:<target>`. It owns every live edge of that job and direction
  in both stages (`analyzer` and `synthesized`), non-viable and
  `NO_RELATIONSHIP` edges included.
- A **finding** is one signal raised about one or more verdicts: its
  *concerns* (every verdict it is about) and its *targets* (the verdicts a
  retire removes; none when the data cannot say which is wrong). A
  **job-level** finding (`stale_prompt`, `stale_model`) is raised about one
  job instead: it concerns no verdict, and its evidence names the job.

`edge_cleaning_runs`, `edge_findings` and `edge_finding_decisions` are
authoritative. The graph is a
projection of them: retiring supersedes edges (`SupersedeFact`), restoring
creates new ones, and nothing is deleted. The package never calls an LLM.

Retiring changes only the graph, so only graph readers (the crosswalk export
and graph queries) stop seeing a retired verdict. `crosscodex results` reads
`vote_summaries`, which cleaning does not touch, so it still lists retired
verdicts, unmarked.

## Reading verdicts

`Engine.plan` loads the tenant's completed jobs (`FindingStore.CompletedJobs`),
then reads each catalog pair with `crosswalk.LoadPair`, once per stage, with
`IncludeNonViable` and `IncludeNoRelationship`. Unscoped, the pairs are every
distinct unordered `(source, target)` catalog pair of those jobs; scoped, the
one pair given. `QueryRelationships` returns only edges without `valid_to`,
so retired edges are never read again. `BuildPairs` joins the two stages so one
`VerdictKey` owns its edges in both. A verdict's type and confidence come from
its analyzer-stage edge, else its synthesized one; its agreement is the first
parsable, finite `agreement_fraction` property, analyzer stage first.

### Tenant-wide jobs read

The `jobs` RLS policy shows only the context user's own jobs. A cleaning pass
judges every verdict of the tenant, so `PGFindingStore.CompletedJobs` drops the
user with `tenant.WithoutUser(ctx)` before reading. Tenant isolation is
unaffected: the context still carries the tenant, the transaction still sets
`app.current_tenant`, and RLS still confines the read to that tenant. The
callers are gated (an admin runs passes, a reviewer decides), and the verdicts
those jobs wrote to the graph are already tenant-visible.

`CompletedJobs` fails closed: a job config without catalogs, a non-NULL
`stage_record` that does not parse (an empty string included), or a job with
no stage completion time is an error naming the job.

## Signals

`Detect` runs every signal and returns findings ordered by signal
(`equivalence_fanout`, `cross_job_contradiction`, `low_agreement`,
`conflicts_with`, `stale_prompt`, `stale_model`), then by concerns. It reads
only `min_agreement_fraction` from the policy; the action is applied later by
`BuildPlan`.

| Signal | Raised when | Concerns | Targets |
|---|---|---|---|
| `equivalence_fanout` | a control's merged verdict (`crosswalk.Merge`) is direct `EQUIVALENT` with two or more controls of one other catalog (`crosswalk.EquivalenceGroups`) | the latest job's verdicts on those pairs | none |
| `cross_job_contradiction` | verdicts of two or more distinct jobs read a pair differently, after reversed verdicts are inverted | every verdict on the pair | every verdict not from `Merge`'s `LatestJob` |
| `low_agreement` | a verdict's agreement is below `min_agreement_fraction` | the verdict | the verdict |
| `conflicts_with` | a verdict's type is `CONFLICTS_WITH` | the verdict | the verdict |
| `stale_prompt` | a compared stage of the job ran a different prompt version than it would now | none: the job | the job's live verdicts in scope, kept only under `retire` |
| `stale_model` | a compared stage's panel changed: an alias added or removed, or a revision that differs while set on both sides | none: the job | the job's live verdicts in scope, kept only under `retire` |

Rules that are easy to get wrong:

- A job's own verdicts, in either direction, never contradict each other. A
  reversed verdict whose type cannot be inverted never equals a forward one.
- `stale_prompt` and `stale_model` raise at most one job-level finding per
  (signal, job), and none for a job without verdicts in the pairs read. Its
  evidence is `job` (ID, completion time, both catalogs' recorded
  `<name> <version>` else the catalog ID, and `verdicts_in_scope`, the count
  of the job's live verdicts read) and no `verdicts`, so its size does not
  grow with the job. Evidence detail `stages` lists every stale stage, sorted
  and comma-separated; `<stage>.recorded` and `<stage>.current` hold the
  compared prompt versions, or the models as sorted `alias=revision` pairs
  joined by `,`. Its catalogs are the job's, and a job maps exactly one
  catalog pair, so a scoped run covering that pair reads all of the job's
  verdicts.
- **Unknown is never stale.** Only stages the current deployment knows are
  compared (`Engine.currentRecords` calls `pipeline.Service.CurrentStageRecord`
  once per stage name; it reports `false` for a stage that is not a registered
  analyzer or renders no prompt). A compared stage whose record is NULL or
  missing is unknown for both signals; for `stale_prompt`, so is an empty
  prompt version on either side. For `stale_model`, a revision unset on either
  side is not a change, because no pin existed to compare.
- `Summary.UnknownPrompt` and `UnknownModel` count jobs, not stages: a job
  adds one when it has at least one unknown compared stage and no stage stale
  for that signal.
- A verdict whose key cannot be expressed as `<job>:<source>:<target>` (a part
  that is empty, holds `:` or exceeds 256 bytes) is excluded from detection
  and counted in `Summary.UnaddressableVerdicts`: no stored finding could name
  it. Catalog ingestion forbids only `/` in item IDs, so such IDs can exist.
- `equivalence_fanout` has no targets, so `pkg/config` rejects `retire` for
  it, and a reviewer's retire must name the verdicts (`retire_verdicts`).
  `BuildPlan` also refuses a retire action on any finding without targets.

### Stage records

`job_stages.stage_record` holds a `StageRecord` as canonical JSON:
`prompt_name`, `prompt_version` and `models` (panel alias to its
`llm.model_revisions` value, `""` when unset). The pipeline writes it when an
analyzer stage that rendered a prompt completes, NULL otherwise, and
`ResetStagesFrom` clears it. `ParseStageRecord` is strict: unknown fields,
trailing data, more than 64 KiB, and aliases or revisions over 256 bytes wrap
`ErrInvalidStageRecord`; `Marshal` refuses what the parser would reject, so a
stored record always reads back.

## Identity and hashing

All hashes are hex SHA-256. `hashParts` prefixes each part with its decimal
length and `:`, so no two part lists share an encoding even when a part holds
a separator or newline.

| Value | Input | Purpose |
|---|---|---|
| `FindingID` | `hashParts(signal, sorted concern keys…)`; for a job-level finding `hashParts(signal, "job", job ID)` | Identity and subject key. Evidence and targets do not affect it, so a re-run updates the stored finding instead of adding one. A job-level finding's ID does not depend on which of the job's verdicts are live or in scope, so retiring or restoring one of them through another finding leaves it, and a decision on it, in place. The kinds cannot collide: a concern key always holds two `:`, `job` none. |
| `EvidenceHash` | canonical JSON of the evidence (declaration order, sorted map keys, UTC times) with each verdict's `edge_ids` removed, and of a job only its ID and completion time | Detects changed evidence. Edge IDs are left out because a restore changes them; including them would re-raise (and under a retire policy, re-retire) every restored finding. Edge changes stay guarded by the two tokens below. A job's `verdicts_in_scope` and catalog names are left out too: the count changes whenever another finding retires or restores one of the job's verdicts, and the names only display the job. A dismissed or restored job-level finding is therefore re-raised only when a stale stage or its recorded or current value changes (or the job's completion time, after a rerun). |
| `StateToken` | `hashParts(status, evidence_hash, sorted edge IDs…)` | Quoted by a decision; refuses a decision on a finding that changed since it was read. |
| plan token | canonical JSON of `{scope, policy, findings}`, `policy` the tenant's effective `CleaningTenantConfig` and each finding `[ID, EvidenceHash, Action, Change, To, EdgeIDs]` in ID order | Binds an apply to its preview. `Summary` is not hashed. |
| policy decision ID | canonical JSON of tenant, finding, verdict, run ID (the plan token), evidence hash, edge IDs, `decided_by` and `decided_at` | Primary key of a run's decision. |
| reviewer decision ID | canonical JSON of tenant, finding, verdict, retire verdicts, rationale, reviewer, expected state, `decided_at`, and for a retire `retire_targets_count` and `retire_targets_sha256` (`hashParts` of the sorted `<job>:<source>:<target>` keys it removes; `0` and `""` otherwise) | Primary key of a reviewer's decision, and the material of its link. A job-level retire expands its targets at decide time, and a later decision overwrites the finding row, so the signed count and hash are what proves which verdicts a retire removed: recompute the hash from the finding's targets as stored right after the decision, or from the retired verdicts. |

Evidence is self-contained: each verdict carries its key, the job's
completion time, both controls' identifier, label and title, both catalogs
(the job's recorded `<name> <version>`, else the catalog ID), the relationship
word, the agreement and its edge IDs; a job-level finding's job carries the
same job facts. A finding stays readable after its jobs, catalogs or edges
are gone.

## Scope

`Scope{SourceCatalog, TargetCatalog}` covers one unordered catalog pair, or
everything when both are empty. Exactly one set is refused
(`ErrInvalidScope`, and `InvalidArgument` at the handler). A finding's catalogs
are the catalog IDs of its first concern's stored source and target, or a
job-level finding's job's; the store compares them with the scope in either
direction. Resume takes no scope:
it projects every outstanding change, and the handler refuses a scope with
`CLEANING_MODE_RESUME`.

## Finding status

| Status | Meaning | Set by |
|---|---|---|
| `reported` | recorded, no action pending | a run, action `report` |
| `pending` | in the review queue | a run, action `human` |
| `retired` | target verdicts superseded | a run, action `retire`; a reviewer's `retire` |
| `dismissed` | a reviewer judged it not a problem | a reviewer's `dismiss` |
| `restored` | a reviewer reversed a retire | a reviewer's `restore` |
| `cleared` | a later in-scope pass no longer detects it | a run |

### Run transitions (`BuildPlan`)

A detected finding is given the status its signal's action wants
(`report` → `reported`, `human` → `pending`, `retire` → `retired`):

| Stored | Detected | Evidence hash | Change | Result |
|---|---|---|---|---|
| none | yes | | `new` | wanted status |
| `cleared` | yes | any | `new` | wanted status |
| `reported`, `pending` | yes | differs | `changed` | wanted status |
| `reported`, `pending` | yes | same, status differs from wanted | `status` | wanted status |
| `reported`, `pending` | yes | same, status as wanted | | omitted |
| `dismissed`, `restored` | yes | differs | `reraised` | wanted status |
| `dismissed`, `restored` | yes | same | | omitted |
| `retired` | yes | any | | omitted |
| `reported`, `pending` | no, catalogs in scope | | `cleared` | `cleared` |
| any other | no | | | omitted |

A job-level finding keeps its targets (the job's live verdicts in scope) and
their edge IDs only when it goes to `retired`; under `report` or `human` it is
planned and stored with neither. That is the plan-time retire expansion: the
expanded verdicts join `Plan.Retire`, deduplicated with other findings'
targets, count against `max_retire_per_run`, and their edge IDs are in the
plan token and in `CheckLive` before the write, so a graph change between
preview and apply still fails on the token.

`BuildPlan` fails when an action is not `report`, `human` or `retire`, a
retire action meets a finding without targets, a stored status is unknown, two
detected or two stored findings share an ID, the plan would retire more
verdicts than `max_retire_per_run` (`ErrRetireCapExceeded`), or a planned
finding would not fit the store (`ErrFindingTooLarge`, see "Writes are
validated against the reader"). The last is checked here, at preview, so no
size limit surfaces first at apply. `Engine.plan` refuses an unknown stored
status before planning.

### Decision transitions (`engine_decide.go`)

| Verdict | Allowed from | Leads to | Graph change |
|---|---|---|---|
| `retire` | `reported`, `pending`, `dismissed`, `restored` | `retired` | supersede the live edges of the retired verdicts |
| `dismiss` | `reported`, `pending` | `dismissed` | none |
| `restore` | `retired` | `restored` | recreate the retired edges |

`cleared` allows no decision. A `retire` removes `retire_verdicts` when given
(each must be one of the finding's concerns), else the finding's targets; it
is refused when neither exists (`ErrRetireNeedsVerdicts`). It re-reads the
finding's catalog pair and refuses a target without a live edge
(`ErrEdgeNotLive`). On a job-level finding the targets expand at decide time
instead: `retire_verdicts` must be among the job's live verdicts in that pair
(`ErrVerdictNotInFinding` otherwise), and without them every live verdict of
the job is retired; a job with no live verdict left is `ErrEdgeNotLive`. The
finding then stores exactly what was retired as its targets and edge IDs, and
a `restore` recreates those edges. The reviewer path has no retire cap; the
cap bounds runs. `retire_verdicts` with `dismiss` or `restore` is
`ErrInvalidDecision`.

## Write order and resume

Every graph change is recorded first and applied second, so a crash between
the two leaves a row that says what is still to do.

**Apply** (`Engine.Apply`):

1. Refuse a half-set scope and any unprojected finding.
2. Recompute the plan; refuse unless its token is the expected one
   (`ErrTokenMismatch`).
3. Refuse without a signer (`ErrSigningDisabled`), and refuse unless every
   edge to retire is live (`CheckLive`). The retire cap was checked while
   planning. Nothing has been written yet.
4. Sign one `cleaning.run` link: material `crosscodex:cleaning-plan` with the
   plan token as digest, one product `crosscodex:finding/<id>` per finding with
   its evidence hash.
5. `ApplyRun`, one transaction: insert the run record (`edge_cleaning_runs`:
   plan token, `Plan.Canonical`, the run link, applied time), upsert every
   planned finding, then insert one policy decision (`retire`,
   `decided_by = 'policy'`, `run_id` = plan token, `decided_at` = the run's
   applied time, the run link as attestation) per finding going to `retired`.
   A plan without findings stops before step 4: it changes nothing, so it is
   neither signed nor recorded. Retired
   findings are written with `graph_applied = false`; others need no graph
   change and are written `true`. Each upsert carries the state it was
   planned from: a new finding must not exist yet (a unique violation means
   another run inserted it), and a stored one is locked `FOR UPDATE` and its
   `StateToken` compared. This closes the race between the token check in
   step 2 and the write; a mismatch is `ErrStateChanged`, returned as
   `ErrTokenMismatch`.
6. Project (below).

**Decide** (`Engine.Decide`): refuse an empty or whitespace-only rationale
(`ErrInvalidDecision`), any unprojected finding and a missing signer; check the transition and the state token; resolve the retire targets
and their live edges; sign one `cleaning.decide` link (material
`crosscodex:cleaning-decision/<id>`, product `crosscodex:finding/<id>` with its
evidence hash); then `FindingStore.Decide` in one transaction:
`require_active_reviewer` (which locks the reviewer row `FOR SHARE`), lock the
finding `FOR UPDATE`, compare its state token, insert the decision, and update
status, targets and edge IDs with `graph_applied = false`. Then project.

**Project** (`Engine.project`, also all of `Resume`): for each finding with
`graph_applied = false`, in ID order:

- `retired`: `RetireEdges` at the `decided_at` of its latest `retire` decision;
- `restored`: `RestoreEdges` with its latest `restore` decision's ID and
  `decided_at` as `valid_from`; the finding's edge IDs become the successors';
- `reported`, `pending`, `dismissed`, `cleared`: no graph change;
- any other status: refused, nothing applied for it.

Then `MarkProjected` sets `graph_applied = true` and the edge IDs. A failure
part-way returns a `*ProjectionError`, which matches `ErrProjectionIncomplete`
and wraps the cause. Its `Public` text says how many findings were applied
out of how many and to run `crosscodex admin cleaning run --resume`; from
`Decide` it also names the decision, which was recorded and stands. The
handler returns `FailedPrecondition` with that text only, so a driver error
in the cause never reaches the caller (CWE-209); `Error` adds the cause for
logs.

While any finding is unprojected, `Preview`, `Apply` and `Decide` are refused
with `ErrUnprojected` (`FailedPrecondition`), because an outstanding change
would alter the graph under a token. `Resume` needs no token and no signer: it
applies only decisions already recorded and attested. Because each change's
time and restored IDs come from its decision, a repeated or concurrent
projection of the same finding writes the same values. Projection is not
serialized, though: the unprojected guard is check-then-act, so a `Decide`
and an `Apply` can project at once, and when two findings share an edge the
later supersede may overwrite its `valid_to` with the other decision's time.

## Graph operations (shared with #230)

`RetireEdges`, `RestoreEdges` and `CheckLive` in `pkg/cleaning/graphops.go`
are the only graph mutations of cleaning. They take edge IDs, not findings, so
that `FeedbackService` (#230) can retire and restore a mapping's verdicts
through them.

```go
func RetireEdges(ctx context.Context, g graphdb.GraphDB, tenantID string, edgeIDs []string, at time.Time) ([]string, error)
func RestoreEdges(ctx context.Context, g graphdb.GraphDB, tenantID string, retiredEdgeIDs []string, decisionID string, at time.Time) ([]string, error)
func CheckLive(ctx context.Context, g graphdb.GraphDB, tenantID string, edgeIDs []string) error
```

- **`RetireEdges`** supersedes each live edge at `at` and returns the IDs it
  superseded, in input order. An already superseded edge is skipped (a second
  `SupersedeFact` would overwrite its `valid_to`). A missing edge wraps
  `graphdb.ErrEdgeNotFound`. An `at` before the edge's `valid_from` is refused,
  since it would invert the validity interval. On error the IDs superseded so
  far are returned; calling again with the same IDs finishes the job.
- **`RestoreEdges`** recreates each retired edge as a new live edge: same
  endpoints, label, properties, determination fields and confidence;
  `Supersedes` set to the retired ID; `valid_from = at`, so an as-of query
  inside the retired gap does not show the mapping; ID
  `graphdb.DerivedID("semantic-match-restored", retiredID, decisionID)`. Keys in
  `graphdb.ReservedEdgeKeys()` are stripped from the copied properties, because
  the driver writes them itself. It returns the live successors' IDs in input
  order. Every input is checked before anything is written: an empty decision
  ID, a zero `at`, a missing edge (`graphdb.ErrEdgeNotFound`), a live edge
  (`ErrEdgeLive`), and an `at` before the retired edge's `valid_to` are refused.
- **Idempotence of restore.** A retired edge keeps at most one live successor:
  a live edge from the same source to the same target whose `supersedes` is
  the retired ID, from this or any other decision, counts as done and its ID
  is returned (the lowest, if several), keeping the first restore's
  `valid_from`. An existing edge with the derived ID (`graphdb.ErrEdgeExists`)
  also counts as done.
- **`CheckLive`** returns an error wrapping `ErrEdgeNotLive` naming the first
  edge that is missing or superseded; any other graph failure is returned as
  is, so an outage is not reported as a missing edge.

The engine passes the decision's own time as `at`, not the current time, so
a retry repeats the same change; other callers need to do the same.

A restored edge has a new ID. The `mapping_id` that `results` reports is
`DerivedID("semantic-match", job, source, target)`, computed from relational
rows, so after a restore it still names the original analyzer edge, which is
superseded. #230 must resolve a mapping to its live edge, not assume the
`mapping_id` is live.

## Append-only runs and decisions

`edge_cleaning_runs` is the record of every applied run, whether or not it
retired anything (#290). A run is keyed by tenant, plan token and applied
time, because a later run can make the same changes from the same state and
so share the token. Its `plan` column holds the canonical plan the token
hashes (scope, effective policy, and each finding's ID, evidence hash, action,
change, new status and edge IDs), and `attestation` the signed `cleaning.run`
link whose material digest is the token and whose products are the run's
findings. To prove which policy and plan moved a finding to `reported`,
`pending`, `cleared` or `retired`, find the run whose link names
`crosscodex:finding/<id>` among its products, check that the SHA-256 of
`plan` is `plan_token`, verify the link, and read the policy from `plan`. No
RPC exposes runs yet; read the table directly. A run row grows with its plan
(every planned finding's edge IDs, and one link product per finding), and
nothing bounds it beyond what the plan's findings already are; a run that
retires findings also copies its link into each decision row.

`edge_finding_decisions` is the record of who decided what, when, and under
which signed request. A policy decision also carries the run's link, so it
verifies on its own without a join. It is append-only because projection reads each
finding's latest decision for the change's time and the restored edge IDs, so
an edited decision would silently change history and make a resumed
projection disagree with the first one. A decision is reversed by recording
another (`restore` reverses `retire`).

Projection does not rebuild the graph from this table. `--resume` only
finishes findings whose `graph_applied` is false, and nothing re-applies
cleaning decisions after a full graph rebuild (the #148 runbook in
`design-principles.md`). After a rebuild, the new jobs' `SEMANTIC_MATCH`
edges are live, while the old findings still read `retired` or `restored`
with `graph_applied = true`, and their `edge_ids` name edges that no longer
exist. Decisions do not carry over to the new jobs' verdicts: a pass under a
`retire` policy retires matching verdicts again, but reviewer decisions must
be made again on the new findings.

Enforcement, from the outside in:

- `app_user` has `SELECT, INSERT` only on `edge_cleaning_runs` and
  `edge_finding_decisions`, and `SELECT, INSERT, UPDATE` on `edge_findings`
  (a finding no longer detected becomes `cleared`, never deleted).
- `refuse_change` triggers refuse `UPDATE`, `DELETE` and `TRUNCATE` on
  `edge_cleaning_runs` and `edge_finding_decisions` for every role, the owner
  included. A plain `TRUNCATE` of runs fails earlier, on the foreign key
  from decisions.
- `CHECK ((decided_by = 'policy') = (run_id IS NOT NULL))`: a policy decision
  names the run that made it, a reviewer's names none. `Decision.Validate`,
  `ApplyRun` (policy decisions only) and `Decide` (reviewer decisions only)
  enforce the same before writing.
- A foreign key ties each decision to its finding, and a policy decision's
  `(run_id, decided_at)` to its run; RLS on all three tables keys on
  `app.current_tenant`. `RunWrite.Validate` refuses a policy decision whose
  run ID or time is not the run's, so the foreign key is the last line.
- A reviewer decision runs `require_active_reviewer` inside its transaction,
  so a deleted or unregistered reviewer cannot decide
  (`ErrReviewerDeleted`, `ErrReviewerNotRegistered`).

Only retirements made by a run get a decision row. A run that moves findings
to `reported`, `pending` or `cleared` updates `edge_findings` and records its
run; the run record is what attests those moves.

### Writes are validated against the reader

Stored JSON is read back strictly (`ParseEvidence`, `decodeKeys`) and rows
cannot be deleted, so one unreadable row would block every read of the
tenant's findings for good. The stores therefore refuse at write time anything
a read would reject: `StoredFinding.Validate` checks the signal, the status,
that a job-level signal's evidence names a valid job ID and it concerns no
verdict (and that no other signal's names a job), that the ID is `FindingID`
of its signal and subject, every verdict key, and that each JSON column
encodes within its bound. Evidence and concerns use `maxStoredJSONBytes`,
half the 1 MiB read bound, because PostgreSQL renders `jsonb` with a space
after every `:` and `,`. Targets and edge IDs list what a retire removed, and
for a job-level finding that grows with the job, so they use
`maxStoredListBytes` (128 MiB), half of `maxListBytes`, which is PostgreSQL's
own `jsonb` limit (2^28-1 bytes): no row the database accepts is refused on
read, and the only cap left on a job's size is the database's. That keeps the
retired edge IDs on the finding row, where projection, `StateToken` and the
plan token already read them, instead of adding a table. A finding over any
bound is refused with `ErrFindingTooLarge` (`FailedPrecondition`), naming the
finding, the column, its size and the bound. For targets and edge IDs the
message depends on the path, because a retire is refused from `retired` and a
finding can therefore be retired in only one subset: `BuildPlan` checks every
planned finding at preview and says to set the signal's action to `report`
or `human` and apply, after which a reviewer may retire one subset that fits
with `--retire-verdict`; `Engine.Decide` checks the finding a retire would
write before signing and says to name fewer verdicts with `--retire-verdict`.
The store's own check (`Validate`, `MarkProjected`) is the last line and only
points back at those two.

Specs exercise the targets and edge IDs bound at a lowered limit through
`SetStoredListLimit` in `export_test.go`, so no unit spec builds a 128 MiB
value; one integration spec checks the real bound against PostgreSQL.
`Decision.Validate` checks the verdict, that request and attestation are
present, and the provenance rule. `RunRecord.Validate` checks that plan and
attestation are present, that the token is the plan's SHA-256 and that the
applied time is set, and `RunWrite.Validate` combines all three with the
run-decision match. The `cleaningtest` fake enforces the same,
and `cleaningtest.FindingStoreContract` runs against both the fake and the
PostgreSQL store.

## Retention

The retention engine does not purge `edge_cleaning_runs`, `edge_findings` or
`edge_finding_decisions`: they are not in `dbAggregates`
(`pkg/retention/purge.go`) and have no foreign key to `jobs` or `catalogs`.
Like `artifact_pair_verdicts`, they are audit records. Because evidence is
self-contained, a finding stays readable after its jobs are purged. A purged
job is no longer a completed job, so its verdicts are no longer read: on the
next pass over their catalog pair, a `reported` or `pending` finding that is
no longer detected without them is `cleared`.

## Handler

`internal/cleaning.Service` checks the role first (`RunCleaning`: admin;
`ListFindings`, `GetFinding`: reviewer or admin; `DecideFinding`: reviewer),
then that `tenant_context` equals the authenticated tenant
(`ErrTenantMismatch`, naming neither tenant), and calls the engine only with
that tenant. The decider is the authenticated subject; no request field names
one. `ListFindings` pages default to 50 and allow at most 1000; a larger or
negative `page_size` is refused, not clamped, as is an unknown signal or
status filter.

Engine sentinels a caller can act on map to `PermissionDenied`, `NotFound`,
`InvalidArgument` or `FailedPrecondition`. The engine's message is kept only
when every leaf of the error is such a sentinel; if a driver error is wrapped
too, only the sentinel's own text is sent. Anything else goes to the
gateway's `publicErrorInterceptor`, which withholds it. Role `all` mounts the
service directly. In a split deployment the graph role runs the engine behind
its listener (`internal/graph.Server`) and the `gateway` role forwards the four
RPCs to it, asserting the caller's identity so the role checks above apply to
the end user; the signing keys, the cleaning stores and the graph are the graph
role's. The engine's current stage record comes from the pipeline role
(`GetCurrentStageRecord`); when that call fails the run is refused with
`Unavailable`. The gateway's `publicError` withholds the message, so the user
sees only an error reference; the gateway log records the full text (naming
`pipeline.endpoint` and `tls.service_identities`) under that reference and the
graph role's log has the cause. Only a caller dialing the graph listener
directly sees the `pipeline.endpoint` text. See [Service Runtime](service-runtime.md#graph-role-listener).

## Telemetry

`WithEngineTelemetry` gives each public engine method a span
`cleaning.<Method>` with `tenant.id` and `cleaning.operation`, and records:

- `cleaning.operation.duration_ms` by `operation` and `outcome` (`ok`,
  `error`). Its count serves as the operations counter, so there is no
  separate one.
- `cleaning.findings.total`: findings an apply wrote, by `signal` and
  `action`.
- `cleaning.findings.pending`: the tenant's `pending` findings, set after a
  successful apply or decision. A failed count is logged, not returned.

`WithFindingStoreTelemetry` gives each store method a span
`cleaning.findingstore.<Op>` and records
`cleaning.findingstore.operations.total` and
`cleaning.findingstore.operation.duration_ms` by `operation` and `status`.
The handler has no telemetry of its own. Neither option has a config field: `attachAllGateway` passes
the daemon's providers. See [Telemetry](telemetry.md#registered-metrics).

## Tests

- Unit and property specs: `pkg/cleaning`, `pkg/cleaning/cleaningtest`,
  `internal/cleaning`, `cmd/crosscodex` (`admin_cleaning_bdd_test.go`) and
  `cmd/crosscodexd` (`cleaning_wiring_bdd_test.go`).
- `task test:integration:cleaning`: the store contract, RLS and the
  append-only decisions against PostgreSQL (`store_integration_bdd_test.go`).
  `pkg/cleaning` is also in `task test:integration:db`.
- Fuzz targets (human-run only): `FuzzParseStageRecord`,
  `FuzzParseVerdictKey`, `FuzzParseEvidence`, `FuzzDecodeKeys` in
  `pkg/cleaning/cleaning_fuzz_test.go`.

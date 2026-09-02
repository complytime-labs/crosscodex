# Data Retention Lifecycle

This document describes how the retention engine works: its pipeline, the
role-based purge carve-out, the archive-before-purge guarantee, the API-first
design, and the tenant-isolation model. For operator/user instructions (CLI,
config, cron) see the "Data Retention & Legal Holds" section of the top-level
`README.md`.

## Overview

The retention engine enforces per-data-class lifecycle policy: expired data is
archived to a secondary store and then purged from the primary store, unless a
legal hold covers it. A scan is always scoped to exactly one tenant.

The design rationale (why lifecycle enforcement is an API-first, per-tenant
operation rather than a batch cron script that reaches directly into the
database) is recorded in
[`docs/superpowers/specs/2026-08-31-data-retention-lifecycle-design.md`](../superpowers/specs/2026-08-31-data-retention-lifecycle-design.md).

## Engine Pipeline

`retention.Engine.Scan(ctx, ScanOptions) (Report, error)` runs a five-stage
pipeline (`pkg/retention/engine.go`). The context MUST already carry the target
tenant (`tenant.WithTenant`); every database path is RLS-scoped and an unscoped
context default-denies.

1. **HoldStore.Active** — load the tenant's active holds (plus global holds; see
   Tenant Isolation below) as of `ScanOptions.Now`.
2. **Collector** — enumerate candidates. `NewDBCollector` reads completed jobs
   and `NewCatalogCollector` reads catalogs, both from relational tables inside a
   tenant transaction; `NewObjectCollector` lists objects under a class prefix in
   the primary object store (attestations). A candidate is emitted only if
   `Policy.Expired(class, tenant, createdAt, now)` is true.
3. **HoldChecker** — drop any candidate covered by an active hold
   (`Hold.Covers`). Held candidates are counted in `Report.Held` and never
   archived or purged.
4. **Archiver** — copy the candidate to the archive store, then read it back and
   compare content hashes (see Archive-Verify Guarantee). On any archive error
   the engine appends to `Report.Errors`, **skips the purge for that candidate**,
   and continues with the next one.
5. **Purger** — delete the candidate from the primary store (see Role-Based
   Purge Carve-Out).

Each successful archive/purge emits an audit record
(`ActionArchive`/`ActionPurge`) via the `AuditPublisher`. Audit-publish failures
are non-fatal and recorded in `Report.Errors`. `ScanOptions.DryRun` runs stages
1–3 and reports what would be archived/purged without mutating anything.

`Report` carries aggregate counters (`Scanned`, `Held`, `Archived`, `Purged`), a
`PerClass` breakdown, and the accumulated non-fatal `Errors`.

### Purgeable classes (audit carve-out)

The engine collects and purges three classes directly: `JobResults` (completed
jobs, relational), `Catalogs` (relational), and `Attestation` (objects);
`tierField` in `pkg/retention/tier.go` maps each to its configured tier. A
`Catalogs` candidate is purged as an aggregate together with its relational
dependents — `classifications`, `controls`, and `embeddings`. `Embeddings` is
therefore **not** collected independently: the table carries no per-row timestamp
(no `created_at` column), so `retention.tiers.embeddings` cannot drive an
independent scan and embedding lifetime is bound to the parent catalog's
retention. (Adding a `created_at` to `embeddings` for independent embedding
retention is a noted follow-up, mirroring the audit-class note below.) The
audit classes — decisions, LLM, and events — are deliberately **not**
engine-purged: `tierField` returns the empty tier for them, which
`Policy.Expired` treats as indefinite. Audit retention is instead enforced by
JetStream stream `MaxAge`, configured under `nats.streams.audit_llm_retention`
and `nats.streams.audit_events_retention` (see
[Audit Streams](audit-streams.md)). Any `retention.defaults.audit_*` tier in
config is accepted for completeness but never drives an engine purge.

## Archive-Verify Guarantee

Purge never runs on data that was not provably archived first. The archiver
(`pkg/retention/archive.go`):

- For object candidates: `Get` from primary, `Put` to archive, then `Get` the
  archived copy back and compare `storage.ContentHash` of source and read-back.
- For relational candidates: serialize to JSON inside a tenant transaction, `Put`
  it to the archive at the deterministic key `db/<class>/<id>.json`, read it back,
  and compare hashes. Every relational class is an **aggregate** — a parent row
  plus its FK-child sets — built by one
  `json_build_object(... json_agg(row_to_json(...)) ...)` query (child sets
  ordered deterministically so an unchanged parent re-archives byte-stably, each
  `COALESCE`d to `[]`):
  - `JobResults`: the completed job plus rows in the four tables that
    `REFERENCES jobs(job_id)` — `job_stages`, `vote_summaries`,
    `analysis_results`, `relationship_candidates` — and `requires_candidates`,
    which is job-scoped data associated by its `job_id` **column** (no FK) rather
    than a true FK child.
  - `Catalogs`: the catalog plus its `classifications` and `controls`. The archive
    doc deliberately **omits `embeddings`** even though the purge deletes them:
    embeddings are derived/regenerable, so they are purge-only. This
    archive⊆purge asymmetry is intentional — do not add embeddings to the archive
    to "match" the purge.

  If the parent row is absent its field is `NULL` (`row_to_json` over no rows) and
  the archiver errors rather than writing an empty document; the guarded parent
  key is per class (`job` for jobs, `catalog` for catalogs). The archive query and
  the purge delete plan are a matched pair (`dbAggregateQueries` in `archive.go`,
  `dbAggregates` in `purge.go`) so — modulo the deliberate embeddings omission —
  the archived set and the deleted set cannot drift.

  The `requires_candidates` / `requires_votes` / `requires_consensus` tables carry
  a `job_id` **column** but have no foreign key to `jobs`, so they never block the
  job delete. `requires_candidates` is the only one written in production
  (`internal/pipeline/candidate_writer.go`), so it **is** folded into the
  `JobResults` aggregate above — archived and purged with its job. `requires_votes`
  and `requires_consensus` have no production writer, so purging them would be
  dead work; they are intentionally left out (revisit if a writer is added).

If the hashes differ the archiver returns `ErrArchiveVerify`; the engine records
the error and leaves the primary copy in place. A silently corrupting archive
backend therefore results in retained primary data and a recorded error — never
data loss. This is covered end-to-end by the corrupting-backend spec in
`pkg/retention/engine_integration_bdd_test.go`.

## Role-Based Purge Carve-Out

Primary data is protected by delete-path immutability triggers: the ordinary
`app_user` role cannot delete completed jobs or other terminal records. Purge is
therefore performed under a dedicated, sanctioned credential:

- Migration 001 creates a `NOLOGIN` group role `retention_purge` and a
  `purge_user` login role that is a member of it. `purge_user` is **not**
  `BYPASSRLS`; it is still subject to tenant isolation. It is the only credential
  the delete-path immutability triggers accept.
- The `Purger` (`pkg/retention/purge.go`) MUST be constructed with a connection
  authenticated as `purge_user`. It sets `app.current_tenant` per candidate and
  deletes within a tenant transaction, so RLS still confines it to one tenant.
- Every purgeable relational class is an aggregate; the purge deletes the FK
  children **before** the parent, in one transaction:
  - `JobResults`: `DELETE FROM <child> WHERE job_id = $1` for each of `job_stages`,
    `vote_summaries`, `analysis_results`, `relationship_candidates`, and
    `requires_candidates` (job-scoped by column, no FK), then
    `DELETE FROM jobs WHERE job_id = $1`.
  - `Catalogs`: `DELETE FROM <child> WHERE catalog_id = $1` for each of
    `classifications`, `embeddings`, `controls`, then
    `DELETE FROM catalogs WHERE catalog_id = $1`. Note `embeddings` is deleted here
    though it is absent from the archive doc (purge-only; see Archive-Verify).

  The children reference only the parent (not one another), so their relative order
  is free; the parent MUST be last, or the FK (which has no `ON DELETE CASCADE`)
  raises `foreign_key_violation`. All table/column names are compile-time
  constants; the candidate id is only ever a bound `$1`. Migration 003 grants
  `purge_user` `DELETE` on `analysis_results` and `relationship_candidates` (001
  already covered `jobs`/`job_stages`/`vote_summaries`); migration 004 grants
  `DELETE` on `controls` (001 already covered `catalogs`/`classifications`/
  `embeddings`); migration 005 grants `DELETE` on `requires_candidates`. None of
  these tables has an immutability delete trigger that rejects `purge_user`, so a
  grant alone suffices.
- The daemon opens this connection from `config.Database.PurgeDSN`
  (`cmd/crosscodexd/bootstrap.go`). When `PurgeDSN` is unset the Purger falls
  back to the app pool so scans still run, but purges fail closed against the
  immutability triggers rather than silently deleting under the wrong role.

## Tenant Isolation Model

Retention is per-tenant end to end; RLS is the enforcement boundary, not
application code.

- **Per-request, per-tenant engine.** A fresh engine is built for a single
  resolved tenant per request (`engineFor`), and the scan context is scoped with
  `tenant.WithTenant` before `Engine.Scan`. `DBCollector`, `archiveDB`, and
  `purgeDB` all begin tenant transactions that set `app.current_tenant`; an
  unscoped context is rejected with `ErrTenantRequired`.
- **`retention_holds` RLS (migration 002).** The `tenant_isolation` policy lets a
  tenant see its own holds **and** global holds (`tenant_id IS NULL`), so a
  global hold blocks every tenant's purge — this mirrors the wildcard semantics
  of `HoldScope` in `pkg/retention/hold.go`. The `WITH CHECK` clause is stricter:
  `app_user` may only write rows for the current tenant and may **not** create
  global (NULL-tenant) holds. Global holds are a privileged-path concept, never
  created through the tenant-scoped admin API.
- **`CreateHold` forces the scope tenant.** The admin service overwrites the
  incoming scope's tenant with the resolved caller tenant
  (`internal/admin/service.go`), so a caller cannot plant a hold for another
  tenant. This is defense-in-depth atop the RLS `WITH CHECK`.
- **On-host one-shot is unauthenticated by design.** `crosscodexd admin
  retention scan --tenant <id>` (`cmd/crosscodexd/admin.go`) runs in-process on
  the host with no RPC authentication, consistent with the `healthcheck` and
  `version` one-shots. It selects its tenant via `--tenant` and scopes the
  context before scanning; RLS still confines every database operation to that
  tenant.

The cross-tenant isolation spec in
`pkg/retention/engine_integration_bdd_test.go` proves this end to end against a
live database: expired data seeded under tenant A is untouched by a scan scoped
to tenant B.

## Tests

- Unit specs: `pkg/retention/*_bdd_test.go` (collectors, policy, hold matching,
  archiver, purger, engine wiring with fakes).
- Integration specs (`//go:build integration`, real PostgreSQL + on-disk
  stores): `pkg/retention/engine_integration_bdd_test.go` and
  `pkg/retention/hold_store_integration_bdd_test.go`. Run with
  `task test:integration:db` (see the memory note on `TMPDIR`/`STORAGE_DRIVER`
  for local podman).

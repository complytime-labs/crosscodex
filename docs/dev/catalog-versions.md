# Catalog Document Versions

Maintainer notes for #204: how versions of one catalog document are stored, how
an import finds its predecessor, and how a version is retired. Usage is in the
README (step 6, "Import a compliance catalog", and the Upgrading entry
"Catalog document versions (#204)"). Retire's retention mechanics are in
[retention.md](retention.md#retiring-a-catalog-version); this page does not
repeat them.

## Lineage Model

A **document** is a tenant-scoped name (the slug) for a requirement document
whose editions are imported over time. Each edition is an ordinary `catalogs`
row, a **version**.

| Where | What |
|---|---|
| `catalog_documents(tenant_id, document_slug, current_catalog_id, created_at)` | One row per document. `current_catalog_id` is the newest version. |
| `catalogs.document_slug`, `version_label` | Set on versions only. A catalog imported without `--document` has both NULL ("unversioned") and is never backfilled. |
| `catalogs.supersedes_catalog_id` | The version that was current when this one was imported; NULL for a document's first version. |
| `catalogs.link_method` | How `supersedes_catalog_id` was established. `CHECK (link_method IN ('declared','metadata','name'))`. |

Migration `002` (`pkg/db/migrations/002_catalog_documents.up.sql`, applied only by `crosscodexd
migrate`) adds all of this. Both lineage references are composite
`(catalog_id, tenant_id)` foreign keys onto `catalogs`, so a cross-tenant link
is impossible by construction. The migration was edited in place, as migration
`001` was before release: a development database already at version 2 from an
earlier build must be recreated (see README Upgrading).

### Document slug versus `catalog_id`

`catalog_id` stays `hash(content, tenant)`. The slug is a separate, human-chosen
grouping, so controls, graph `Control` nodes and the result cache are keyed
exactly as before and are untouched by versioning. Consequences:

- Identical content is one catalog. Re-importing it under the same document is
  a refresh that keeps its place (current does not move, label and lineage are
  unchanged).
  An explicit `--version-label` that differs from the stored label is refused
  with `ErrVersionLabelConflict` (`FailedPrecondition`) before anything is
  written: relabelling is not supported. The check runs in the service against
  the pre-upsert read, not inside the store transaction.
- Content already imported without a document is attached to the document
  rather than duplicated.
- Content already in a different document is refused with
  `ErrCatalogInOtherDocument` and nothing is written. A catalog belongs to one
  document.

### `link_method` and the #286 extension point

**#286 (the three-source link chain, admin-controlled support state, chain
RPCs) is not implemented.** The only link this code writes is
`link_method = 'declared'`: the importer asserted the successor relationship by
importing under an existing slug. `linkVersionTx` in `internal/catalog/store.go`
is the single writer.

The `metadata` and `name` values are allowed by the CHECK constraint so that
#286 can add links without another migration, but nothing writes or reads them.
Nothing consumes `link_method` for behaviour; it is only displayed (`catalog
list` / `inspect --json`). Do not assume a `metadata` or `name` row can exist
until #286 lands, and extend `linkVersionTx` (or its replacement) rather than
writing the column elsewhere.

## Version Display in Job Reports and Exports

`pkg/catalogversion` is the one formatter of `slug @ label (date · hash6)`
(`Format`, and `OfCatalog` for a `pb.Catalog`); `internal/pipeline` may not
import `internal/catalog`, so the formatter lives in `pkg/`.

- **Recorded at job creation.** `pgCatalogNameReader` (`internal/pipeline`)
  reads `document_slug`, `version_label`, `created_at`, `content_hash` and
  `supersedes_catalog_id` with the name and version, and sets
  `CatalogSnapshot.version_display` and `supersedes_catalog_id` for a
  versioned catalog. `jobconfig.Label` carries both.
- **Shown.** `CatalogSummary.version_display` / `supersedes_catalog_id` are
  filled from the recorded label, or, for a job that recorded none, from the
  catalog's current row (`internal/results` `readAnalysis`) or `GetCatalog`
  (crosswalk in `internal/gateway`, `run status` in the CLI). `results export`
  JSON carries them; `run status` prints the display (cleaned with
  `CleanDisplay`) and its `--json` adds both keys only for a versioned side.
  The CSV export columns are unchanged.
- Unversioned catalogs render as before: the fields are empty, which the
  proto-JSON export shows as empty strings.

## Import Flow

`ParseCatalog`, `StreamDocument` and `SubmitDocument` carry `document_slug` and
`version_label` and return `version`, `superseded_catalog_id` and
`change_report`.

1. **Validate (untrusted input).** `ValidateDocumentSlug`: 1-63 characters of
   `a-z`, `0-9`, `-`, not starting with `-`. `ValidateVersionLabel`: at most 64
   characters; rejects control and format characters, U+2028/U+2029 and
   `<`, `>`, backtick. Labels are stored and displayed escaped and never reach
   a prompt raw.
2. **Resolve the label.** `ResolveVersionLabel`: explicit `--version-label`,
   else the OSCAL `metadata.version`, else `<import date> · <6-hex content
   hash>`. The stored label is returned as `ParseCatalogResponse.version_label`
   and recorded as the gateway ingest attestation's `version_label` by-product,
   so an OSCAL-derived or fallback label is attested too.
3. **Store.** `PGStore.UpsertCatalogVersion` writes the catalog, lineage,
   containers and controls in one transaction; a failed import stores nothing.
4. **Report.** The change report compares the new version's `controls` rows
   with the superseded version's (`ListControlsForCatalog`), never the graph.

`DiffControls` matches controls by `Identifier` only (no text similarity). A
shared identifier is `changed` when the SHA-256 of its statement differs.
`IDsUnstable` is set when fewer than a tenth of the larger version's
identifiers are shared; the lists are still correct but are not an edit
history, and no matching is attempted.

## Concurrency Model

Imports under one slug serialise on the `catalog_documents` row, so the
versions form one chain with exactly one current:

1. The row is created with `INSERT ... ON CONFLICT DO NOTHING RETURNING`. A
   concurrent first import waits on the winner's commit and gets no row back.
2. The loser then reads `current_catalog_id` with `SELECT ... FOR UPDATE`, sets
   `supersedes_catalog_id` to it and moves `current_catalog_id`, in the same
   transaction as the catalog and control rows.
3. `upsertCatalogTx` locks an existing `catalogs` row before `document_slug` is
   read, so a catalog cannot be attached to two documents concurrently.

Different slugs, and imports without a slug, do not contend.

## Retire

`catalog retire <slug>@<label>...` and the admin-only
`CatalogService.RetireCatalogVersion` RPC (`internal/catalog/retire.go`). The
resolve, protection, archive and purge steps are described in
[retention.md](retention.md#retiring-a-catalog-version). What a maintainer must
keep true:

- **All or nothing.** Every label is resolved and every protection checked for
  every version before anything is archived. One protected version refuses the
  whole request, and the purge is a single `Purger.PurgeBatch` transaction as
  `purge_user`.
- **Protections.** The document's current version; an active hold covering it
  as a `ClassCatalogs` candidate; an active hold naming a job whose stored
  config references it; an unfinished job (status not `completed`, `failed`
  or `cancelled`, see `terminalJobStatuses` in `internal/catalog/store.go`)
  whose config references it; a job whose config references it that has
  attestation objects under `attestation/<job_id>/`. The import's own
  `attestation/ingest/...` does not count.
- **Lineage stays a chain.** The purge re-points the successor's
  `supersedes_catalog_id` to the retired version's predecessor. The scheduled
  retention purge does the same. Its catalog collector marks an expired
  current version `Protected`, so the scan counts it as held and never
  archives, purges or audits it; the purge's own `ErrCatalogIsCurrent` refusal
  stays as defence in depth for a version that becomes current mid-scan.
- **Archive before purge.** A failed archive deletes nothing.
- **Graph nodes stay.** Retire does not touch graph `Control` / `Container`
  nodes; the graph is a materialized view and the retention purge leaves it too.
- **Tenant scoping.** Errors name only the caller's holds, jobs and documents.

### Fails closed when

- the server has no purge connection (`database.purge_dsn` unset) or no archive
  backend, for example the standalone `gateway` role: `FailedPrecondition`
  saying so, nothing changed;
- the document or a label is unknown (`NotFound`), or a label is shared by two
  versions (refused);
- any version is protected (`FailedPrecondition`, `ErrVersionProtected`, every
  blocker listed);
- a job config is NULL or cannot be read by `jobconfig.ExtractCatalogs` (the
  job is reported as unreadable and blocks, since it might reference the
  version).

## Known Limitations

- **Hold race window.** Holds are loaded and checked, then the versions are
  archived and purged in a later transaction. A hold placed in between is not
  seen by that retire. Holds are placed by administrators and the window is the
  archive time; there is no lock between the hold store and the purge.
- **Job-config scan cost.** Which catalogs a job uses is decided by
  `jobconfig.ExtractCatalogs`, not a SQL filter, so retire reads every stored
  job config of the tenant (`PGStore.JobsUsingCatalogs`) and then lists
  attestation objects per referencing job. Cost grows with the tenant's job
  count, not the number of versions retired.
- **Duplicate control identifiers.** If an identifier repeats within one
  version, `DiffControls` compares the last occurrence; earlier duplicates are
  invisible to the report. The report is a review aid, not an audit record.
- **Identifier-only matching.** A control renumbered between versions is
  reported as one removed and one added.
- **Retire is explicit only.** There is no automatic pruning of old versions.

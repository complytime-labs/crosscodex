# CrossCodex

A Go-first, multi-service compliance mapping platform that compares compliance standards, maps relationships between requirements, and stores the complete graph with full traceability.

CrossCodex delivers composable microservices, provider-agnostic LLM integration, and multi-tenant security with defense-in-depth.

______________________________________________________________________

> LLM WARNING: This project was written with LLM (AI) assistance.

______________________________________________________________________

## Status

CrossCodex is in early development. All foundational, domain, and service packages are implemented and tested. The CLI provides approximately 22 commands across project, catalog, run, results, prompt, version, and completion groups with daemon connectivity and embedded single-node mode. See [Development](#development) below to build from source and run tests.

## Upgrading

The graph storage hardening in #148 makes these breaking changes:

- Tenant IDs are limited to 52 characters (3-52 lowercase letters, digits and hyphens). Existing longer tenants stop working and must be re-provisioned under a new ID; see the "Tenant IDs longer than 52 characters" note linked below for the detection query and steps.
- Derived edge ID formats changed, so existing graphs do not deduplicate against newly written edges.
- Temporal graph properties (`valid_from`, `valid_to`, `analyzed_at`) use a new fixed-width encoding.
- Node and edge properties that use a reserved key (such as `id` or `valid_from`) are rejected.
- `BulkCreateEdges` rejects requests with more than `graph.max_bulk_edges` edges (default 1000).
- `CreateEdge` and `BulkCreateEdges` reject a `temporal.confidence` outside 0 to 1.
- The never-populated `BulkCreateEdgesResponse.errors` field (field 3) is removed and reserved; `BulkCreateEdges` reports failures only as the RPC status.
- The unused `tenants.allowed_tenants` config key is removed; delete it. A config file that still sets it fails to load (see unknown config keys below); `CROSSCODEX_TENANTS_ALLOWED_TENANTS` is ignored.

Graphs written before this change must be rebuilt. Follow the "Upgrade note (#148)" in [docs/dev/design-principles.md](docs/dev/design-principles.md#graph-data-model).

The pre-release schema migrations are collapsed into one baseline, `001_initial_schema`. A database migrated by an earlier build cannot be upgraded; `crosscodexd` refuses it as newer than itself. Drop it (for the compose deployment, `task deploy:down VOLUMES=1`) and recreate it with `crosscodexd migrate`.

Unknown config keys:

- A config file (system, user, drop-in, profile, project or `--config`) or CLI flag override that sets a key that is not a configuration field now fails to load instead of being silently ignored. This catches misspellings (`pipeline.stage_timout`) and keys that were removed (`analysis.relationship.top_k`, `tenants.allowed_tenants`). One error lists every unknown key in every file layer and CLI flag override, so all of them can be fixed in one pass, each with its dotted path, `file:line:column`, a "did you mean" suggestion when a valid key is close, and the valid keys at that level; a removed key says what replaced it. Example: `unknown configuration key "pipeline.stage_timout" at /etc/crosscodex/config.yaml:2:3: did you mean "stage_timeout"? (valid keys under pipeline: addr, endpoint, max_concurrent_jobs, stage_timeout)`. `tenant_overrides` entries are checked the same way, and a global-only key set in one names the keys that can be overridden. Environment variables are not checked: a `CROSSCODEX_*` variable that matches no key is still ignored.
- Loading now reports every problem at once instead of stopping at the first: every invalid value in every section, values of the wrong type (text for a number, a value where a section belongs), and the invalid values alongside any unknown keys. A value of the wrong type is reported against the file, line and column (or CLI flag, or environment variable) that set it, such as ``pipeline.max_concurrent_jobs at /etc/crosscodex/config.yaml:4:24: cannot unmarshal !!str `many` into int``, instead of a line in the merged configuration. A float setting set to `.nan` is rejected. The CLI prints the problems one per line under a single summary line. Each error keeps its message (with `(set in <file>)` where the value came from a file, now also for `analysis.engine`, `analysis.requires`, `analysis.artifacts`, `analysis.artifact_adjudication`, `analysis.candidates`, `worker`, `pipeline` and `retention`), joined one per line. A setting whose rule involves another, such as `tls.mode mutual` needing `tls.cert`, `tls.key` and `tls.ca`, is reported once, under the setting that imposes the rule.
- A top-level key that only holds a YAML anchor, such as the compose idiom `x-common: &common`, is an unknown key and is rejected. Define the anchor on the first place that uses it, inside a real section, and refer to it from the others; anchors do not reach across files:

  ```yaml
  tls:
    targets:
      llm: &mtls
        mode: mutual
        ca: /etc/crosscodex/ca.pem
      nats: *mtls
      db:
        <<: *mtls
        cert: /etc/crosscodex/db.pem
  ```

- `<<` merge keys and aliases now take effect over the compiled defaults and earlier layers. Before, `logging: {<<: {level: debug}}` loaded with the default level `warn`, because the default's explicit `level` beat the merged one. Within one mapping, a key set explicitly still wins over a merged one, and in `<<: [*a, *b]` the earlier entry wins, as YAML specifies.
- `crosscodex config set <key> <value>` and the new `crosscodex config unset <key>` edit only the user config, and write it only if the configuration loads with the change, every other layer and `--profile` included (only that profile; others are not checked). A misspelled key, an invalid value, a key that names a section or list, a path through an alias (`<path> is an alias to another section`), or a change to a section or value whose anchor an alias in the file refers to (the error names the aliases), is refused with the reason, and the file is left byte-identical. When a change depends on another setting (such as `tls.mode mutual` on `tls.cert`, `tls.key` and `tls.ca`), set that one first. The file is replaced atomically, keeping its permissions and owner (a new one is created `0600`; a config owned by another user is refused unless the command can give the new file that owner), and a symlinked user config is written through to its target; a dangling symlink is refused.
- When the configuration does not load, every command fails with the error, except the `crosscodex config` commands: they print it as a warning and still run. If the user config is the cause, `config unset` removes an unknown key and `config set` fixes a value, one problem per change: a change is written if it removes at least one problem and adds none (the remaining ones are printed as a warning), and refused if it adds a problem or replaces a bad value with another bad one. A rule spanning settings counts as one problem under the setting that imposes it, so `tls.mode mutual` stays a problem until `tls.cert`, `tls.key` and `tls.ca` are all set: set those first, or `tls.mode` back to `off`. If some problem has no setting path (a document whose root is not a mapping, such as a YAML list; a file that does not parse is refused outright, fix its syntax by hand), a change is written only if it makes the configuration load. If a problem's value comes from another layer (system config, drop-in, profile, environment variable), set and unset refuse and name it: fix that file first. A layer that only loads together with the user config (a drop-in sets `llm.gateway_mode: true`, the user config sets `llm.gateway_url`) does not count as broken. A map key a dotted path cannot hold (one containing `.`, `[`, `]` or `"`) is written quoted, both in errors and in `config set`, `get` and `unset` keys, for example `crosscodex config unset 'llm.tenant_overrides["a.b"]'`. `config show` and `config get` have no resolved configuration to print, so they report the error once instead.

Tenant administration (#31):

- The gateway refuses every non-health request, with `PermissionDenied`, from a tenant whose `tenants` row is missing, suspended or deleted. The initial schema creates the `tenant_admin` role that `crosscodexd admin tenant` connects as, so tenants can only be provisioned after `crosscodexd migrate` runs. Once crosscodexd has migrated, set the role's password and `CROSSCODEX_DATABASE_TENANT_ADMIN_DSN`, then run `crosscodexd admin tenant create` for each authenticated tenant before sending it traffic; requests from that tenant are refused until then. Roles `all` and `gateway` refuse to start until `crosscodexd migrate` has provisioned `tenants.default_tenant`. See [Tenant administration](deploy/README.md#tenant-administration).
- The gateway refuses, with `PermissionDenied`, a request whose `tenant_context` names a tenant other than the one its client certificate authenticates. Before, the gateway replaced the named tenant with the certificate's and answered for that one, so `crosscodex catalog list --tenant acme` with a certificate for `default` listed `default`'s catalogs, and `run start --tenant acme` started a job for `default`. Omit `--tenant` (it defaults to the certificate's tenant), or connect with a certificate issued for the tenant you name.

Analysis stage limits (#237):

- `pipeline.stage_timeout` now caps every analyzer stage. Its default is 24h (was 30m, unused), and `0` is rejected: a config that sets `stage_timeout: 0` or more than `168h` fails to load. Raise it if a catalog's stage legitimately runs longer than 24h.
- `analysis.engine.task_timeout` changed meaning. Before, it was one deadline for a stage's whole batch of LLM tasks, so any real catalog failed after `task_timeout`. Now it is an idle timeout across the whole worker pool: a stage fails only when no worker anywhere reports progress for that long. Total time per stage is capped by `pipeline.stage_timeout` instead (default 24h).
- Split deployments: each LLM worker remembers an abandon for its own host's `pipeline.stage_timeout`, and `worker.max_abandoned_collections` is likewise per worker host. Set `pipeline.stage_timeout` on worker hosts to the same value as on pipeline hosts, or larger. A smaller value makes workers forget abandons early and run tasks nobody reads.
- Mixed versions: workers from before this release publish no progress ticks, so a new pipeline with old workers still fails a stage queued behind another stage at `task_timeout`. Upgrade workers with, or before, pipelines.
- Analyzer `enabled` settings now take effect. Before, every job registered all five analyzers whatever `analysis.<analyzer>.enabled` said, and each ran with whatever models it had: classification and embedding ran even when disabled; artifacts, requires and relationship ran whenever their `models` were set, so a disabled analyzer with models still billed its LLM calls, and with the defaults (`enabled: false`, `models: []`) they ran with zero LLM tasks, so jobs completed with no mappings and no sign that analysis was off. Now a disabled analyzer gets no stage and makes no calls. What changes for you:
  - Classification and embedding default to enabled, so a default configuration still runs them; one that set them `false` no longer runs them.
  - Artifacts no longer runs unless `analysis.artifacts.enabled: true`. A configuration that set `analysis.artifacts.models` without enabling it used to extract artifacts and now does not.
  - Relationship and requires likewise run only when enabled. Both default to off, so a stock deployment produces no mappings until you enable `analysis.relationship` (see [Analyzer `enabled` settings](#analyzer-enabled-settings)).
  - A configuration that enables an analyzer without what it needs no longer loads: embedding without classification, or requires or relationship without embedding or without an enabled `analysis.candidates.generators` entry.
  - A job left running across the upgrade, or across a change to these settings, fails when it resumes if its analyzers differ from the enabled ones, naming each one. Start a new job.
- `crosscodex run cancel` now records the cancellation. Before, the cancelled job's context was also used to write its final state, so every write failed: the job stayed `RUNNING` and resumed at the next daemon start. Stages running at the cancel end with the new stage status `cancelled`. A daemon shutdown still leaves its jobs `RUNNING` for resume, and no longer publishes a `cancelled` job state for them.
- When an analyzer stage times out, fails to dispatch, or its job is cancelled, LLM workers drop that stage's still-queued tasks instead of calling the model for results nobody reads; a resumed or retried job's new run is unaffected. Only workers and pipelines from this release do this: an older worker runs every task (and sends no progress ticks, see above), and a newer worker runs tasks from an older pipeline normally. See [Control Messages: Abandoned Collections](docs/dev/audit-streams.md#control-messages-abandoned-collections).

SDK and prompt changes:

- `natsbus.Client` gained `PublishWorkerProgress` and `SubscribeWorkerProgress`. Implementations outside this repo (`pkg/natsbus` is public) must add them.
- `pkg/natsbus` subject builders reject job IDs longer than 128 bytes with `ErrInvalidJobID`.
- The shipped `requires` prompt is now 1.1.0 and renders `| Control Domain: "${source_ancestor}"`; `${source_ancestor}` is the bare domain name, no longer a pre-formatted suffix. A user or project prompt override of `requires` still using `Level: ${source_level}${source_ancestor}` renders e.g. `Level: tacticalAccess Control`. Prompt overrides are not version-checked, so update them by hand.

Attestation fails closed (#240):

- An attested pipeline job now completes only once its whole attestation is stored: a signed layout, a link per analyzer, synthesis and graph, a chain that verifies, and the bundle. Before, every attestation failure was logged and ignored, so jobs completed with a partial attestation or none. A job that cannot be attested now fails with an error that names the step and object and says what to do. See [Cryptographic Attestation](#cryptographic-attestation).
- `attestation.enabled` now takes effect, per tenant through `attestation.tenant_overrides`. When it is `true` (the default) for any tenant, roles `all` and `pipeline` refuse to start unless that scope's `private_key_path` and `public_key_path` are both set, load, and are one key pair; with `tls.fips.enabled` the key must be on a FIPS-approved curve. Before, the daemon refused only empty global paths, even with attestation disabled, and a key that did not load started the daemon and then left every job unattested. The `deploy/` stack and `test/e2e` already supply keys. A configuration of your own that does not: generate a key pair (`task deploy:certs` writes one to `deploy/certs/`) and set both paths, or set `attestation.enabled: false` to run jobs unattested, which the daemon logs at startup.
- `attestation.include_byproducts`, the per-tenant `expiry_duration` and key paths, and FIPS mode now apply to the pipeline's attestations; they were ignored.
- Analyzer link digests are now SHA-256 of each analyzer's stored result. Before, every production analyzer's link recorded `sha256` of the analyzer's name, so the chain attested nothing about results. Bundles stored before this change still verify their signatures but not against results.
- A job left running across the upgrade resumes as before; if its synthesis or graph stage completed without a stored link, it fails and says to start a new job.
- New settings `attestation.storage_retries` (default 3) and `attestation.storage_retry_delay` (default 2s) bound how long an attestation write is retried before the job fails.
- Every layout step and link is bound to its job: it records `job:<tenant>/<job-id>` with the digest of the job's stored config, and verification refuses a link or bundle from another job. Before, nothing signed named the job, so with the shared global key one job's attestation verified as another's.
- `crosscodex results verify` now verifies a job's whole stored bundle and requires `--public-key`, `--tenant` and `--job-id`; it prints the job and its config digest. Before, it verified a single link and reported `Status: Structure valid` without a key; given a bundle it failed.
- New command `crosscodex run retry <job-id>` (gateway RPC `RetryJob`, `POST /v1/jobs/{job_id}/retry`): `--from-failure` resumes a failed or cancelled job in place, otherwise a new job starts with the same config. Before, retrying needed the pipeline's own RPC, which role `all` does not serve. A failed or cancelled job with no failed or cancelled stage (one whose attestation could not be stored) now resumes instead of being refused, and a job whose failure a retry cannot fix (an attestation that does not verify, changed analyzers, no catalog) is refused with `FailedPrecondition` and left unchanged.
- `jobs` gains an `error_class` column, folded into the pre-release baseline `001_initial_schema`. A database created by an earlier build lacks it; `crosscodexd` now refuses to start on such a database, saying to recreate it (`task deploy:down VOLUMES=1`, then `crosscodexd migrate`).
- Cancelling and retrying: a retry of a job whose cancelled run is still stopping is refused (`FailedPrecondition`, "still stopping") until `crosscodex run status` shows it is no longer running, so a retry never runs beside the cancelled run; concurrent retries of one job start it once. Embedded mode now refuses `run cancel`, since the job is run, and can only be stopped, by the daemon. `JobProgress` gains `cancelled_steps`, and `run status` prints the completed, failed and cancelled stage counts.
- `gateway.PipelineBackend` gains `RetryJob`. Implementations outside this repo must add it. `attestation.Generator.VerifyChain` takes the expected subject and returns a `*VerifiedChain`, and `VerifiedLayout.KeyIDs` lists only the keys whose signatures verified.
- `pipeline.New` takes a `pipeline.Attestors` (built by `pipeline.NewAttestors`) in place of an `attestation.Generator` and `config.AttestationConfig`. `attestation.Generator.VerifyChain` now requires the layout and one link per layout step (`ErrMissingLink`, `ErrUnexpectedLink`) and verifies every signature; callers that passed a nil layout or a partial chain get an error.

LLM result reuse (#243):

- The pre-release baseline `001_initial_schema` changed again. It adds the tables `llm_result_cache`, `job_cache_entries` and `job_cache_stats`, a `(job_id, tenant_id)` unique key on `jobs`, and the column `job_stages.cache_attempt`. `crosscodexd` refuses to start on a database created by an earlier build and names the missing column. Recreate it: `task deploy:down VOLUMES=1`, start the stack again, then run `crosscodexd migrate`.
- New API fields, all additive: `JobConfig.no_cache`, `StartAnalysisRequest.no_cache` and `PipelineJob.cache_summaries` (`GetJob` only), with the enums `ResultCacheMode` and `ResultCacheAttestation`. Existing clients are unaffected. `run status --json` gains a `cache` key.
- New `jobs.error_class` values: `result_cache_unavailable` and `llm_unavailable` (both retryable) and `result_cache_corrupt`. `run retry` refuses a `result_cache_corrupt` job in both forms, except a new job from a job that already had `--no-cache`. The remedy is a new job with `--no-cache`.
- Each analyzer's attestation link now pins the material `<analyzer>.cache_manifest`, and the job's layout requires it. The link also records the byproducts `result_cache.mode`, `result_cache.reason` and `result_cache.attempt`.
- Changing the attestation key pair, globally or through a tenant override, ends reuse of results from earlier jobs. Those results are recomputed, and nothing is flagged as corrupt.
- A tenant whose jobs are not attested (`attestation.enabled: false`, globally or for the tenant) gets no reuse, not even of its own earlier attempts. Startup logs this for each such scope.
- If the pipeline and worker processes run with different `llm.default_model` or `llm.embedding_model` values, the cache stays correct but reuse drops. A request with no model alias is keyed by the pipeline's default, while the worker answers with its own, and a result is stored only when the model that answered is the one the job probed.
- `GetJob` and `ListJobs` now set `PipelineJob.error` on a failed job, so `run status` prints an `Error:` line and `run status --json` an `error` object (one line, control characters removed, at most 2048 bytes); before, both were always empty. The message is written for the job's owner: a failed stage's own message; the full error when the job's analyzers changed, its config names no catalog or its target catalog has no control with a statement; otherwise `job <id> failed because of an internal error (error class <class>)`, the commands that run it again as its class allows (resume it with `run retry --from-failure`, or start a new job, with `--no-cache` for `result_cache_corrupt`), and the trace ID (or, with tracing off, the job ID) to give your operator, who finds the full error in the daemon's `job failed` log line. Each stage's error (`JobStage.error`) follows the same rule, and its full error is in the `stage failed` or `job failed` log line. A job that failed before this change keeps the error it recorded, which can include internal detail such as a database error, and `GetJob` and `ListJobs` now return it to the job's owner and to admins.

Identity lifecycle (#195):

- Tenants and SAME_AS verdicts can no longer be deleted by any role short of the database owner or a superuser deliberately disabling triggers (see [Identity Lifecycle](docs/dev/design-principles.md#identity-lifecycle)). Retire a tenant or reviewer with the soft-delete commands under [Tenant administration](#tenant-administration).
- `adjudicate artifacts decide` requires a registered reviewer; register reviewers with `crosscodexd admin reviewer add`.

Cross-catalog analysis (#210) makes these breaking changes:

- `crosscodex run start` requires `--target <catalog-id>`; `run start --source X` alone is refused before connecting. Pass `--target X` for the previous behaviour (analysis within one catalog), or another catalog's ID to map X's controls onto that catalog's. `--bidirectional` with `--target` equal to `--source` is refused. A job that failed before this change recorded `crosscodex run start --source X` (no `--target`) in its error, which the new CLI refuses; `run status` still shows the error as recorded, and a `run retry` refusal quotes it, then ends with a command built from the job's config (`--source X --target X`). See [8. Start an analysis](#8-start-an-analysis).
- `StartAnalysis` and `CreateJob` reject a request without `target_catalog_id`, or with `bidirectional` when the target equals the source, with `InvalidArgument`. Both refuse a source or target that is not one of the caller's tenant's imported catalogs with `NotFound`, before any job is stored.
- A job created before the upgrade has no target in its stored config. It keeps running, resumes and retries within its one catalog.
- `CreateJob` records each catalog's name and version in the job's config (`JobConfig.source_catalog` / `target_catalog`), overwriting whatever the caller sent, so `run status` and `results export` keep showing the labels the job was created under after a catalog is re-imported under another name. A job created before the upgrade has none: both commands show the catalog's current name, and say so.
- `ListJobMappings` responses gain an `analysis` field (`JobAnalysis`: the job's source and target catalogs and direction). `results export` JSON output gains an `analysis` key, and `results export --format csv` appends nine job-level columns after `is_viable`: `job_source_catalog_id`, `job_source_catalog_name`, `job_source_catalog_version`, `job_target_catalog_id`, `job_target_catalog_name`, `job_target_catalog_version`, `job_bidirectional`, `job_source_catalog_snapshot` and `job_target_catalog_snapshot`. A `*_snapshot` column is `true` when that side's name and version were recorded when the job was created, and `false` for a job created before this change, whose labels are the catalog's current ones; JSON carries the same as `snapshot` on each catalog. Scripts that read CSV columns by position must account for them. A CSV cell taken from a catalog (control and catalog IDs, names, versions) that would start with `=`, `+`, `-` or `@` now gets a leading `'`, so a spreadsheet does not run it as a formula; strip it if you compare IDs.
- `run status` prints `Source catalog`, `Target catalog` and `Direction` lines, and its `--json` output an `analysis` object.
- Import records a catalog's OSCAL `metadata.version`. Catalogs imported earlier show the version as "not recorded" until they are imported again.
- A crosswalk job whose target catalog has no non-section control with a statement fails before any analyzer runs, with the new `jobs.error_class` value `empty_target_catalog`. `run retry` refuses it (`FailedPrecondition`) with or without `--from-failure`, since a retry or a new job with the same config reads the same catalog; start a new job with another target (re-importing the target with statements gives it a new catalog ID).
- The shipped `requires` (1.2.0), `relationship` (1.1.0), `artifacts` (1.1.0) and `classify` (1.1.0) prompts treat catalog values as untrusted data. Each control's ID, framework, type, level and control domain are one field per line inside `<source_metadata>` / `<target_metadata>` (`<control_metadata>` for `artifacts`), with no quotes around the domain. `${source_framework}` / `${target_framework}` no longer appear in the system message. The system message says that tag contents are data, not instructions. Every prompt value, catalog values and the `artifact_same_as` (now 1.2.0) artifact names and samples, is NFKC-normalized, so fullwidth and small-form look-alikes such as `＜` fold to ASCII, and is cleaned and has `&`, `<` and `>` entity-escaped. The limits apply to the escaped text: statements keep their character limits, and short values get at most 200 bytes. Prompt overrides are not version-checked, so update them by hand. Results can shift against earlier runs.

Mapping on in the compose stack:

- `deploy/config.yaml` now enables `analysis.relationship` and `analysis.requires` on the gateway's `default` alias, with the `semantic` candidate generator. Before, both were off, so a job on the compose stack completed with zero mappings. A job now makes pair calls as well: about source controls × 10 × 6 (relationship 3 samples + requires 3), so it takes longer and costs more (see [deploy/README.md](deploy/README.md#what-an-analysis-runs-and-what-it-costs)). Set `enabled: false` there to go back. If you renamed the `default` alias in the gateway config, rename it in both `models` lists.
- The compiled defaults are unchanged: a `crosscodexd` with no config file still runs no pair analyzers.

Mapping confidence:

- A relationship mapping's confidence is now the panel's agreement times the mean `analysis.relationship.confidence_weights` weight of the winning votes' reported `CONFIDENCE` (see "Confidence" under [Read a job's mappings](#9-read-a-jobs-mappings)). Before, it was the agreement alone, so a one-sample panel gave every mapping 1.000 and `--min-confidence` and `synthesis.confidence_threshold` filtered nothing. Jobs run after the upgrade report lower confidences than earlier jobs on the same catalogs, and with the default weights and threshold a one-sample panel's LOW mappings are no longer viable. Compare runs from the same build only. Set all three weights to 1 to get the old values back.
- The agreement alone is a new `agreement_fraction` property on each `SEMANTIC_MATCH` graph edge and in the relationship analyzer's stored results.
- Cached LLM results are still reused: the prompts and requests are unchanged, and confidence is computed from the cached responses when a job aggregates them. Changing `samples_per_model` does change the requests (a panel of more than one sample uses `sampling_temperature` instead of 0), so the compose stack's switch to 3 samples sends its relationship and requires requests to the model again once.
- New config key `analysis.relationship.confidence_weights` (`high`, `medium`, `low`). A config that sets a weight outside (0, 1], or a lower level above a higher one, fails to load.

Consensus thresholds:

- The compiled default `consensus_threshold` of `analysis.requires` and `analysis.artifact_adjudication` is now 0.66 (was 0.67). A panel decides when votes for the majority / valid votes ≥ the threshold, and 2/3 is 0.666…, so at 0.67 a three-sample panel decided only when unanimous: a two-of-three requires vote wrote no REQUIRES edge, and a two-of-three adjudication left the pair undecided. Now two of three decide. Expect more REQUIRES edges and fewer undecided artifact pairs; pairs already closed as undecided stay undecided. A config that sets 0.67 explicitly keeps the old behavior.

Analyzer-stage export:

- `results export --stage analyzer` no longer reports a viability, because that stage has none. JSON mappings omit `viabilityScore` and `isViable` (before: `0` and `false`), and the CSV `viability_score` and `is_viable` cells are empty (before: `0.000` and `false`). The columns stay where they were. The `synthesized` stage is unchanged.

Certificate generation:

- The deploy PKI generator moved from `deploy/certs/gen.go` to `deploy/certgen/gen.go`, because a regeneration now replaces the whole `deploy/certs/` directory. Keep nothing else in `deploy/certs/` (or in a custom `CERTS_DIR`).
- `deploy/certgen/gen.go` and `internal/testcerts/gen.go` keep a set that verifies instead of always regenerating, and they verify more: key/certificate pairs, the required SANs, the fingerprint, the attestation keypair (deploy) and at least a day of validity left. A set that fails any check is replaced on the next `task deploy:certs` or `task test:integration:generate-certs`; you no longer delete `deploy/certs/.fingerprint` by hand to pick up a new SAN.
- The embedded daemon's `$XDG_STATE_HOME/crosscodex/pki/` is generated the same way. A `pki/` directory holding certificates without a `.fingerprint` (not generated by the CLI) is no longer overwritten when invalid; the CLI stops with the reason instead. Remove the directory to get a generated set.

Model answers cut off at `max_tokens`:

- A model call that returns no answer because the model stopped at the analyzer's `max_tokens` now fails its stage with error class `llm_output_truncated`. Reasoning models do this when thinking uses the whole budget, and classification's budget is 20 tokens. Before, the empty answer was dropped and the job completed with those controls' results missing. Any such task fails the stage, even when others answered, so a job no longer completes with silent gaps. The task is not retried, because the same request gets the same reply.
- The job's message names the setting to raise (`analysis.classification.max_tokens` and so on) and points to turning the model's reasoning off in the gateway instead (see "llama.cpp, vLLM or another OpenAI-compatible server" in [deploy/README.md](deploy/README.md)). The class is retryable: after either fix (restart `crosscodexd` after changing its config), `crosscodex run retry --from-failure <job-id>` resumes the job.
- Artifact adjudication records such a reply as the pair's failure (`model gave no answer within max_tokens`), and the LLM-assisted catalog structurer refuses it. A partial answer cut off at `max_tokens` is unchanged: the analyzer's parser judges it.

## Quick Start

**Prerequisites**: Go >= 1.23, Task (taskfile.dev), container engine (podman or docker)

### 1. Build from source

```sh
task build
```

### 2. Start the development database

```sh
task dev:up
```

This starts PostgreSQL with AGE graph extension and pgvector.

### 3. Configure the database connection

The dev database requires mutual TLS. Copy the full DSN that `task dev:up` prints:

```sh
<!-- secretlint-disable-next-line @secretlint/secretlint-rule-database-connection-string -- documentation example with dev-only credentials (user: postgres, password: integration, host: localhost) -->
crosscodex config set database.dsn "postgres://postgres:integration@localhost:15432/crosscodex_test?sslmode=verify-full&sslrootcert=.test-output/certs/ca.pem&sslcert=.test-output/certs/client.pem&sslkey=.test-output/certs/client-key.pem"
```

### 4. Download OSCAL catalogs

```sh
task fetch:oscal-docs
```

This downloads official NIST OSCAL catalogs to the `catalogs/` directory.

### 5. Initialize a project

```sh
crosscodex project init
```

### 6. Import a compliance catalog

```sh
crosscodex catalog import catalogs/NIST_SP-800-53_rev5_catalog.json
```

Import stores and parses the catalog and prints its catalog ID. It does not start analysis.

### 7. List and inspect catalogs

```sh
# List all imported catalogs
crosscodex catalog list

# Inspect a specific catalog (use the ID from the list output)
crosscodex catalog inspect <catalog-id>
```

### 8. Start an analysis

An analysis maps the controls of a source catalog onto a target catalog (a crosswalk, for example CSF 2.0 onto SP 800-53):

```sh
crosscodex run start --source <catalog-id> --target <catalog-id>
crosscodex run status <job-id>
```

- **Direction.** Pairs run source to target: each source control is compared with its most similar target controls. NIST IR 8477 relationship types are directional ("A is a subset of B" is not "B is a subset of A"). `--bidirectional` also maps the target's controls onto the source's. For target to source only, swap `--source` and `--target`.
- **`--target` is required.** To analyse within one catalog (for example to find overlaps and REQUIRES dependencies in a catalog that carries no links of its own), pass its ID to both: `crosscodex run start --source <id> --target <id>`. Within one catalog, controls are already paired in both directions, so `--bidirectional` is refused there.
- **What runs.** The job classifies, embeds and extracts artifacts for the controls of both catalogs, then runs the pair analyzers (`requires`, `relationship`) over the candidate pairs. Each pair prompt names both requirements' frameworks, taken from the catalog names. A crosswalk whose target has no non-section, non-withdrawn control with a statement fails before any analyzer runs.
- **Cost.** Sections and withdrawn controls (OSCAL `status` prop `withdrawn`, stored as the control prop `status`; their `incorporated-into` / `moved-to` targets are kept too) make no LLM calls, and `embedding` also skips controls without a statement. For every other control of both catalogs (a job within one catalog counts it once), each enabled per-control analyzer makes: `classify` 1 call, `embedding` 1 per model in `analysis.embedding.models`, `artifacts` `models` × `samples_per_model`. Pair calls ≈ source controls × `top_k` (the semantic generator's `config.top_k` under `analysis.candidates.generators`, default 10) × panel size (`models` × `samples_per_model`), summed over the enabled pair analyzers (`analysis.requires`, `analysis.relationship`). `--bidirectional` adds the reverse direction, whose sources are the target's controls, so pair calls become ≈ (source + target controls) × `top_k` × panel size, summed over the same analyzers: mapping CSF (about 100 controls) onto SP 800-53 (about 1,000) costs about 11 times the one-way pair calls, not twice. `crosscodex catalog list` and `catalog inspect` show each catalog's control count (sections and withdrawn controls included, so an upper bound; SP 800-53 rev 5 has about 200 withdrawn) before you start; `run status` shows both sides' while the job runs.
- **Reuse.** LLM results are reused across your tenant's jobs; other tenants never see them. A request identical to one an earlier job sent is answered from the cache instead of the model. Identical means the same rendered prompt, model alias, temperature, token limit and sample, answered by the same model and gateway deployment at the same `llm.model_revisions` revision.
  - **Which model.** Before analysis, each job makes one probe call per distinct model alias and kind (a completion with `max_tokens: 1`, or one short embedding) to learn which model the gateway serves. The model name the gateway returns and its deployment ID (LiteLLM's `x-litellm-model-id` response header, which changes when the model behind the alias changes) are part of every cache key, so a model change is a miss. A result is stored only when its call reports the same model and deployment as the probe. So an alias load-balanced across several gateway deployments is cached only for the deployment the job's probe reached: results from the others are sent to the model and not stored. To cache every deployment, give each its own alias. An alias is not cached in that job when its probe returns no model name, or returns only the alias itself and no deployment ID: a change of the model behind it could not be detected. To cache such an alias, set `llm.model_revisions.<alias>` and change it whenever the model behind the alias changes.
  - **Trust.** A result is reused only through a signed cache checkpoint. When a stage stops, the daemon signs one checkpoint over everything that stage run stored and reused, with the job's attestation key. Before reusing a result, the stage verifies the signature of the producing run and checks that its records still match what was signed. A result edited after it was stored, even by a database superuser, is never used: the stage fails as `result_cache_corrupt`. A run with no checkpoint supplies nothing, so its requests are sent to the model again. That covers a crashed run, a tenant without attestation, and a checkpoint signed by a key the daemon no longer trusts.
  - **Cost.** An unchanged re-run sends no analysis call. It costs:
    - one probe per distinct alias and kind, per run or resume
    - one signature and one object-storage write each time a stage of a cached analyzer stops
    - for each distinct earlier job whose results a stage reuses, once per stage: one object-storage listing, plus one object read and signature check per checkpoint stored for that job and analyzer (one per stopped stage run)
    - in each lookup batch (`analysis.result_cache.lookup_batch_size` keys) that reads results of such a job, one database re-read of the records of its signed attempts, which the batch checks against their checkpoints
    - on each resume or `retry --from-failure`: one object-storage listing per analyzer that kept no cache records
    - each `run status` (the only command that reads cache summaries): at most one signed object read per analyzer (a checkpoint, or the analyzer's attestation link), plus one read of the stored results when an analyzer kept no cache records
  - **Seeing reuse.** `run status` shows, per analyzer, the results reused against the calls sent, and the model behind each alias. It names the earlier jobs you created that supplied results, and only counts other users' jobs. It shows only figures a signed record vouches for. An analyzer without one shows as not attested, or as in progress while the job runs; one that completed in an earlier run that recorded no cache attempt shows `cache use not recorded for this stage (signed as unrecorded)`. If object storage cannot be read, `run status` still shows the job, with the affected analyzers not attested. In embedded mode, `run status` prints `Result cache: shown only when connected to crosscodexd` instead of the lines.
  - **Fresh runs and the kill switch.**
    - `--no-cache` sends every call to the model for a fresh, reproducible run, and still stores its results. The job's attested config records the flag.
    - Operators can turn reuse off with `analysis.result_cache.enabled: false`, per tenant through `tenant_overrides`.
    - A model update that the gateway's model name and deployment ID cannot show, such as an Ollama re-pull of the same tag, does not cause misses by itself. Change `llm.model_revisions` to force them.
  - **Failures.** Each message ends with the command to run and a trace ID for your operator.
    - The cache cannot be read: the stage stops before any LLM call (`result_cache_unavailable`, retryable).
    - A cached result fails its integrity check: it is never used and the stage stops (`result_cache_corrupt`). Start a new job with `--no-cache`, which replaces the cached results.
    - The model gateway cannot be reached for the probe: the job stops before analysis (`llm_unavailable`, retryable).
  - **Retention.** Cached results are purged with the job that produced them, and with either catalog whose text they carried.
- **Seeing what ran.** `crosscodex run status <job-id>` shows the source and target catalogs (name, version, ID and control count) and the direction. `crosscodex results export` carries the catalogs (ID, name, version) and direction: JSON under `analysis`, CSV in the `job_*` columns, which describe the job, not the row (in a bidirectional job a row's source control can come from the job's target catalog). Names and versions are the ones recorded when the job was created; for a job created before #210 they are the catalog's current ones, which `run status` marks "(current name; not recorded when the job was created)" and export marks with `snapshot: false` / `job_*_catalog_snapshot` `false`. A version shows as "not recorded" when import stored none. A CSV cell taken from a catalog that would start with `=`, `+`, `-` or `@` gets a leading `'` so a spreadsheet does not run it as a formula. JSON key names differ between the two commands: `run status --json` uses snake_case (`source_catalog`), while `results export` renders `analysis` and each mapping as proto JSON, in camelCase (`sourceCatalog`, `mappingId`), as mappings were before #210; only its top-level `job_id` is snake_case.

`run start` prints the job ID. Analysis runs in the `crosscodexd` daemon; embedded mode refuses it (see [Deployment Modes](#deployment-modes)).

`crosscodex run cancel <job-id>` asks for confirmation. When stdin is not a terminal (a script, pipe or CI job) it refuses and cancels nothing, even if `y` is piped in; pass `--yes` to cancel without the prompt. Embedded mode refuses it: only the daemon running the job can stop it.

A cancelled job ends with status `CANCELLED`, and the stages that were running end `cancelled`; it is not resumed automatically. `crosscodex run retry --from-failure <job-id>` resumes a cancelled job from its first cancelled stage, as it resumes a failed job from its first failed one, keeping the stages that completed. A failed or cancelled job with no failed or cancelled stage, such as one whose attestation bundle could not be stored, resumes from where it stopped. A job whose failure a retry cannot fix (its attestation does not verify, its analyzers changed, its config names no catalog, its target catalog has no control with a statement, a cached result it reused failed its integrity check) is refused, unchanged, with the reason, the error as the job recorded it, and the command that starts a new job. Without `--from-failure`, `run retry` starts a new job with the same config and prints its ID, except for a job whose target catalog has no control with a statement or whose cached result failed its integrity check, which it refuses because the new job would fail the same way. The remedy for the second is a new job with `--no-cache`, which `run retry` does start for a job that already had it. A job stored as `bidirectional` within one catalog runs without the flag, whether resumed or retried as a new job. `run retry` asks for no confirmation: it deletes nothing and re-runs only stages that did not complete. A cancelled job keeps writing its cancellation for a moment after `run cancel` returns; until it stops, `run retry` is refused, saying to retry once `crosscodex run status <job-id>` shows it is no longer running. Embedded mode refuses it, as it refuses `run start`. A job is `COMPLETED` only once its attestation bundle is stored, so a cancel during that last step still cancels it; once the job is being recorded as completed, `run cancel` is refused (`FailedPrecondition`) and says to check `crosscodex run status`. A job interrupted by a `crosscodexd` shutdown is not cancelled: it stays `RUNNING` and resumes from its completed stages when the daemon starts again, including one stopped while its bundle was being stored.

### 9. Read a job's mappings

Once a job has completed, read its control mappings. Both commands take the job ID and the same filters:

```sh
# Count mappings by relationship type
crosscodex results summary <job-id>

# Export viable synthesized mappings as proto JSON (enum names, all fields)
crosscodex results export <job-id> --output mappings.json

# Export one control's high-confidence EQUIVALENT mappings as CSV
crosscodex results export <job-id> --format csv --control <control-id> --type equivalent --min-confidence 0.8
```

| Flag | Applies to | Meaning |
|------|------------|---------|
| `--stage synthesized\|analyzer` | both | `synthesized` (default) is the output after synthesis's confidence threshold and per-control cap, with a viability score per mapping. `analyzer` is the raw relationship analyzer consensus, with no viability: `export` omits `viabilityScore` and `isViable` from JSON and leaves the `viability_score` and `is_viable` CSV cells empty. |
| `--min-confidence <0-1>` | both | Only mappings at or above this confidence. |
| `--type <type>` | both | Only this relationship type; repeat for several. Case-insensitive. `NO_RELATIONSHIP` is excluded unless named here. |
| `--control <id>` | both | Only mappings whose source or target is this control. |
| `--include-non-viable` | both | Also return pairs synthesis filtered out (`synthesized` stage only). By default only viable mappings are returned. |
| `--format json\|csv` | `export` | Output format (default `json`). |
| `--output <file>` | `export` | Write to a file instead of stdout. |
| `--json` | `summary` | Structured output. |

A job that is still pending or running returns an error saying mappings are available once it completes. A failed or cancelled job never produces mappings; start a new job. An unknown job ID returns not found. `mapping_id` equals the ID of the mapping's `SEMANTIC_MATCH` graph edge. `REQUIRES` is not a mapping and is not returned.

**Confidence.** A mapping's confidence (0 to 1) is the relationship panel's agreement times how sure the agreeing votes said they were: (votes for the winning type / valid votes) × the mean of `analysis.relationship.confidence_weights` over the winning votes' reported `CONFIDENCE` (HIGH 1.0, MEDIUM 0.67, LOW 0.33 by default; a vote that reports none counts as LOW). Three samples that agree, all HIGH, give 1.0; two of three, HIGH and MEDIUM, give 0.557; a one-sample panel gives exactly its vote's weight. The winning type is still chosen by vote count alone. `--min-confidence` and `synthesis.confidence_threshold` (default 0.5) filter on this value, so with the default weights a one-sample panel's LOW mappings fall below the threshold. The agreement alone is kept as the `agreement_fraction` property of each `SEMANTIC_MATCH` graph edge.

Run `crosscodex --help` to see all available commands, or `crosscodex <command> --help` for command-specific usage.

## Architecture

The target architecture consists of seven core services that can run embedded in a single process or distributed across multiple hosts. Today the monorepo provides implemented infrastructure (`pkg/config`, `pkg/db`, `pkg/storage`, `pkg/natsbus`, `pkg/tlsconfig`, `pkg/authn`, `pkg/tenant`, `pkg/telemetry`, `pkg/llmclient`) and implemented domain packages (`pkg/analyzer`, `pkg/attestation`, `pkg/oscal`, `pkg/graphdb`, `pkg/vectordb`, `pkg/prompt`). Implemented services include `internal/worker` (LLM task execution), `internal/synthesis` (viability ranking and quality diagnostics), `internal/graph` (compliance relationship graph), `internal/analysis` (analysis engine), `internal/catalog` (OSCAL catalog management), `internal/gateway` (API gateway), `internal/pipeline` (job orchestration), and the analyzer plugins (`internal/analyzer/classify`, `internal/analyzer/embedding`, `internal/analyzer/relationship`).

```mermaid
%%{init: {'theme': 'base', 'themeVariables': {
  'primaryColor': '#2f6dab',
  'primaryTextColor': '#1e1e1e',
  'primaryBorderColor': '#7c8ba1',
  'lineColor': '#7c8ba1',
  'edgeLabelBackground': '#eef2f8',
  'tertiaryColor': 'transparent',
  'tertiaryTextColor': '#7c8ba1',
  'tertiaryBorderColor': '#7c8ba1',
  'clusterBkg': 'transparent',
  'clusterBorder': '#7c8ba1',
  'titleColor': '#7c8ba1',
  'noteBkgColor': '#eef2f8',
  'noteTextColor': '#1e1e1e',
  'fontFamily': 'system-ui, sans-serif'
}, 'themeCSS': '.node .nodeLabel{color:#ffffff!important;fill:#ffffff!important;}'}}%%
flowchart TD
    subgraph external ["External Sources"]
        docs["Documents<br/>PDF, DOCX, HTML"]
    end
    subgraph processing ["Processing Pipeline"]
        ingestion["Ingestion Service<br/>Python + Docling"]
        catalog["Catalog Service<br/>Go + OSCAL"]
        analysis["Analysis Engine<br/>Go + Plugins"]
        llm["LLM Workers<br/>Go + AI Models"]
    end
    subgraph orchestration ["Orchestration"]
        synthesis["Synthesis<br/>Quality Ranking"]
        pipeline["Pipeline<br/>Go + NATS"]
    end
    subgraph datalayer ["Data Layer"]
        graphdb["Graph Store<br/>PostgreSQL + AGE"]
        vectordb["Vector Store<br/>pgvector"]
    end
    subgraph infra ["Infrastructure"]
        tenant["Tenant Isolation"]
        tls["TLS / mTLS"]
        authn["Authentication"]
        telemetry["Telemetry<br/>OpenTelemetry"]
        attestation["Attestation<br/>in-toto"]
    end
    docs --> ingestion --> catalog --> analysis --> llm
    llm --> synthesis --> graphdb
    pipeline --> analysis
    pipeline --> synthesis
    graphdb --> pipeline
    analysis --> vectordb
    infra -.-> processing
    infra -.-> orchestration
    infra -.-> datalayer

    classDef sysA fill:#2f6dab,color:#ffffff,stroke:#7c8ba1
    classDef sysB fill:#1d7848,color:#ffffff,stroke:#7c8ba1
    classDef sysC fill:#7457b8,color:#ffffff,stroke:#7c8ba1
    classDef sysD fill:#2d747e,color:#ffffff,stroke:#7c8ba1
    classDef sysF fill:#5c6a82,color:#ffffff,stroke:#7c8ba1
    class ingestion,catalog,analysis,llm sysA
    class synthesis,pipeline sysB
    class graphdb,vectordb sysC
    class tenant,tls,authn,telemetry,attestation sysD
    class docs sysF
```

### Service Responsibilities

| Service             | Purpose                                         | Technology          |
|---------------------|-------------------------------------------------|---------------------|
| **Ingestion**       | Multi-format document conversion via Docling    | Python gRPC service |
| **Catalog**         | OSCAL parsing, document structuring, validation | Go                  |
| **Analysis Engine** | Host for analyzer plugins, DAG execution        | Go                  |
| **LLM Workers**     | Horizontally scalable LLM task execution        | Go                  |
| **Synthesis**       | Ranking, viability weighting, quality metrics   | Go                  |
| **Graph**           | openCypher queries via Apache AGE on PostgreSQL | Go                  |
| **Pipeline**        | Job orchestration, state tracking, retry logic  | Go                  |

The Ingestion service lives in its own repository, [complytime-labs/crosscodex-ingestion](https://github.com/complytime-labs/crosscodex-ingestion). The gateway does not call it yet: it stores uploads unchanged, and the Catalog service imports only OSCAL JSON. Wiring Docling conversion into imports is tracked in issue [#214](https://github.com/complytime-labs/crosscodex/issues/214).

### Deployment Modes

- **Embedded** -- All services in one process with auto-bootstrapped mTLS. Requires PostgreSQL with AGE and pgvector extensions (start with `task dev:up`). Local filesystem for object storage. Catalog import, list, and inspect work for OSCAL JSON documents. Analysis does not run in embedded mode: `crosscodex run start` fails with an error that says to start `crosscodexd` and set `cli.endpoint`, and no job is stored. Run `crosscodexd migrate` before starting `crosscodexd` against a database. `crosscodexd` (default `--role=all`) bootstraps a DB pool and NATS client and runs the graph, worker, and gateway/pipeline roles together in one process; analysis result persistence is durable end-to-end at the library layer (`internal/pipeline`, `pkg/analyzer`).
- **Production (compose)** -- Single-host production stack with full mutual TLS across all services (PostgreSQL, NATS, the LLM gateway, CrossCodex gateway). One-command bring-up via `task deploy:up` after populating `deploy/.env` and generating certificates with `task deploy:certs`. The default LiteLLM gateway runs behind a stunnel sidecar to provide end-to-end mTLS; `LLM_GATEWAY=praxis` selects Praxis AI instead (spike). See [deploy/README.md](deploy/README.md) for setup, first-use CLI commands, and operations.
- **Distributed** -- Services scale independently with external PostgreSQL cluster (AGE + pgvector), NATS cluster with JetStream, and S3-compatible object storage. Multi-host deployment and FIPS 140 container images tracked in issue [#17](https://github.com/complytime-labs/crosscodex/issues/17).

See [Service Runtime Guide](docs/dev/service-runtime.md) for the `--role` flag, role precedence, health-check wiring, the shared `nats.url` every split role must set, and gateway startup preconditions.

## Configuration

CrossCodex follows XDG Base Directory conventions:

```
$XDG_CONFIG_HOME/crosscodex/
  config.yaml                    # User-level defaults
  profiles/
    local.yaml                   # Single-node overrides
    distributed.yaml             # Cluster overrides
  credentials/                   # API keys, certificates (mode 0600)
  tenants/                       # Per-tenant configuration

Project directory:
  .crosscodex/
    config.yaml                  # Project-specific overrides
    prompts/                     # Custom prompt templates

Additional XDG directories:
  $XDG_DATA_HOME/crosscodex/
    prompts/                     # User prompt template layers
  $XDG_STATE_HOME/crosscodex/
    pki/                         # Embedded daemon auto-generated TLS certificates
    daemon.pid                   # Embedded daemon PID and port
```

The embedded daemon generates `pki/` on first use and replaces it whole when it is invalid or within a day of expiry; concurrent CLI invocations share one set. To use your own certificates instead, put `ca.pem`, `ca-key.pem`, `server.pem` (with a `localhost` SAN), `server-key.pem`, `client.pem` and `client-key.pem` in `pki/` without a `.fingerprint` file. The CLI then uses them as long as they verify and never replaces them; if they don't, it stops with the reason. Remove the directory to go back to a generated set.

### Configuration Resolution Order

Values merge in ascending priority (last wins):

1. Compiled defaults
1. System config (`/etc/crosscodex/config.yaml`)
1. System drop-ins (`/etc/crosscodex/conf.d/*.yaml`)
1. User config (`$XDG_CONFIG_HOME/crosscodex/config.yaml`)
1. User drop-ins (`$XDG_CONFIG_HOME/crosscodex/conf.d/*.yaml`)
1. Profile (`--profile local`)
1. Project config (`.crosscodex/config.yaml`)
1. Environment variables (`CROSSCODEX_*`)
1. CLI flags (highest priority)

### Key Configuration Examples

```yaml
# LLM Gateway
llm:
  gateway_url: "http://localhost:4000"  # roles pipeline, worker and all refuse to start if this doesn't answer
  default_model: "qwen3:8b"
  timeout: 30  # seconds per LLM request; 0 means the 30s default. Raise it for a self-hosted server that loads models on demand or reasons before answering (see "Tuning a self-hosted model server" in deploy/README.md)
  probe_timeout: 0  # seconds for the per-job model probe, the first call to reach the model; 0 means llm.timeout. Raise it to cover a cold model load without stretching every analysis call's timeout (global-only)
  model_revisions:                      # alias → revision label, part of every result-cache key (global-only); change it to force cache misses. Required to cache an alias the gateway names only by itself, with no deployment ID
    default: "2026-11-01"

# Storage
storage:
  objects:
    backend: local                # local | s3

# Database
database:
  dsn: "${DATABASE_DSN}"          # app_user, e.g. postgres://app_user:example-password@localhost:5432/crosscodex
  migration_dsn: "${MIGRATION_DSN}"  # schema owner, used only by `crosscodexd migrate`, e.g. postgres://owner:example-password@localhost:5432/crosscodex
  extensions: [age, vector]

# TLS (Global Default)
tls:
  mode: "mutual"                  # off | server-only | mutual
  ca: /etc/crosscodex/tls/ca.crt
  cert: /etc/crosscodex/tls/server.crt
  key: /etc/crosscodex/tls/server.key

# Attestation (roles all and pipeline refuse to start if an enabled scope's keys are missing or do not load)
attestation:
  enabled: true                   # false: jobs complete unattested (logged at startup)
  private_key_path: /etc/crosscodex/attestation-key.pem   # ECDSA (SEC1 "EC PRIVATE KEY")
  public_key_path: /etc/crosscodex/attestation-pub.pem    # PKIX "PUBLIC KEY" of the same key
  expiry_duration: 8760h          # layout validity; `results verify` rejects an expired layout
  storage_retries: 3              # retries of a failed attestation write, [0, 10]; global-only
  storage_retry_delay: 2s         # wait before each retry, [0, 1m]; global-only
  tenant_overrides:
    sandbox:
      enabled: false              # this tenant's jobs run unattested

# Multi-tenant X.509 certificate-to-tenant mapping
tenants:
  enabled: true
auth:
  x509_mappings:
    - match:
        organization: "Acme*"
        org_unit: "Engineering"
      tenant: acme-engineering
      roles: [admin, writer]
    - match:
        san_email: "*@partner.com"
      tenant: partner-org
      roles: [reader]

# Analysis
analysis:
  engine:
    task_timeout: 5m                     # (0, 30m]; fail a stage when neither its own results nor any worker progress arrive for this long
    max_retries: 3                       # Redispatches of a failed task
    retry_backoff: 1s                    # Base delay before a redispatch
  classification:
    enabled: true                 # Runs the classify analyzer; embedding needs it
    model: ""                     # Inherits from llm.default_model
    max_text_length: 2000
    temperature: 0.0
    max_tokens: 20
  embedding:
    enabled: true                        # Runs the embedding analyzer; needs classification; requires and relationship need it
    models: ["snowflake-arctic-embed2"]  # Embedding model names; storage is model-agnostic (see docs/dev/embeddings.md), switching models needs no schema migration
    max_chars: 1500                      # Max runes before truncation
    batch_size: 50                       # Controls per batch call
  relationship:                          # The analyzer whose results are a job's mappings
    enabled: false                       # Off in the compiled defaults (deploy/config.yaml turns it on); needs embedding and an enabled candidates generator
    models: []                           # Panel models; required when enabled
    confidence_weights:                  # Weight of each vote's reported CONFIDENCE in a mapping's confidence; each (0, 1], high >= medium >= low (global-only)
      high: 1.0
      medium: 0.67
      low: 0.33                          # Also used for a vote that reports no CONFIDENCE
  requires:
    enabled: false                       # Off in the compiled defaults (deploy/config.yaml turns it on); needs embedding and an enabled candidates generator; finds REQUIRES edges, which are not mappings
    models: []                           # Panel models; required when enabled
    samples_per_model: 1                 # Votes per model per pair; odd unless allow_even_samples
    allow_even_samples: false            # Allow an even samples_per_model
    sampling_temperature: 0.3            # [0, 2]
    max_tokens: 300                      # Max tokens per LLM reply
    consensus_threshold: 0.66            # [0.5, 1]; minimum confidence fraction
    max_error_rate: 0.34                 # [0, 1]; maximum fraction of votes that can fail
    max_source_chars: 1500               # Max runes of source control text
    max_target_chars: 800                # Max runes of target control text
  candidates:
    min_embedding_coverage: 0.8          # Minimum fraction of a job's non-section controls that have a statement and must have an embedding before candidate generation runs; below this, the candidate_generation stage fails the job (fail-closed)
    embed_model: "snowflake-arctic-embed2"  # Embedding model candidate generation reads; must be one of analysis.embedding.models
    generators:                          # Candidate generators; none by default, and with none enabled no candidate pairs are produced
      - name: semantic                   # Most-similar controls by embedding
        enabled: true
        weight: 1.0                      # [0, 1] in weighted aggregation; 0 or unset means 1.0
        config:                          # Optional; unknown keys are rejected at load
          top_k: 10                      # Most-similar targets kept per source control; positive integer (default 10)
          min_similarity: 0              # Drop targets below this cosine similarity x 100; [0, 100] (default 0)
      - name: keyword                    # Targets whose text contains a keyword (case-insensitive substring)
        enabled: false
        weight: 1.0
        config:
          keywords: [policy, procedure]  # Non-empty list of non-empty strings; omit to use the built-in list below
          # Built-in list: policy, procedure, plan, program, framework,
          # establish, define, designate, inventory, baseline, register, catalogue, catalog, architecture,
          # strategy, governance, charter
      - name: level                      # Targets at a higher abstraction level; takes no config keys
        enabled: false
        weight: 1.0
  artifact_adjudication:                 # `crosscodexd admin adjudicate artifacts`
    enabled: false                       # The command refuses to run until true
    models: []                           # Panel models; required when enabled
    samples_per_model: 3                 # Votes per model per pair; odd unless allow_even_samples
    allow_even_samples: false            # Allow an even samples_per_model
    sampling_temperature: 0.3            # [0, 2]; 0 is used with one sample per model
    max_tokens: 200                      # Max tokens per panel reply
    consensus_threshold: 0.66            # [0.5, 1]; below it (or on a split) the pair is undecided
    max_error_rate: 0.34                 # [0, 1]; more failed votes than this fails the panel (0 disables)
    max_attempts: 5                      # Failed panels before a pair is abandoned
    lease_duration: 15m                  # How long a run holds each leased pair, and each verdict it or `decide` projects; must be positive even when enabled is false
    lease_batch: 10                      # Pairs leased per round trip
    panel_concurrency: 4                 # Completions in flight per pair
    retry_base_delay: 1m                 # Wait after a pair's first failed panel, doubling per failure
    retry_max_delay: 24h                 # Cap on that wait; at least retry_base_delay
  result_cache:                          # LLM result reuse across a tenant's jobs (#243); needs attestation for the tenant
    enabled: true                        # Kill switch; per tenant via tenant_overrides. run start --no-cache skips lookups for one job
    lookup_batch_size: 1000              # [1, 10000]; cache keys per lookup query
    tenant_overrides: {}                 # tenant ID → {enabled, lookup_batch_size}

worker:
  max_abandoned_collections: 10000        # [1, 100000]; abandoned stages each worker remembers so it can drop their queued LLM tasks; raise it if more stages are abandoned within one stage_timeout; per worker host (global-only)

pipeline:
  stage_timeout: 24h                      # (0, 168h]; hard cap on each analyzer stage, which worker progress never extends; fails a stage whose task was lost; worker hosts must set the same or a larger value (they keep abandons that long)

graph:
  max_bulk_edges: 1000                    # Most edges per BulkCreateEdges request, range [1, 10000]; larger requests are rejected (global-only)

synthesis:
  confidence_threshold: 0.5               # Minimum confidence for viable mappings (see "Confidence" under "Read a job's mappings")
  max_mappings_per_control: 10            # Maximum mappings per source control
  viability:
    type_mismatch_factor: 0.8             # Penalty for different classification types
    skip_level_factor: 0.7                # Penalty for non-adjacent abstraction levels
    integral_to_factor: 1.1               # Boost for INTEGRAL_TO contribution type
  assessment:
    iqr_good: 20.0                        # IQR threshold for good embedding spread
    iqr_poor: 10.0                        # IQR threshold for poor embedding spread
    no_rel_high: 0.97                     # NO_RELATIONSHIP rate upper bound
    no_rel_low: 0.80                      # NO_RELATIONSHIP rate lower bound
    contested_warn: 0.20                  # Contested pairs warning threshold
    actionable_warn: 0.30                 # Actionable coverage warning threshold
```

#### Analyzer `enabled` settings

`analysis.classification.enabled`, `analysis.embedding.enabled`, `analysis.artifacts.enabled`, `analysis.requires.enabled` and `analysis.relationship.enabled` decide which analyzers a job runs. A disabled analyzer gets no job stage, never runs and makes no LLM calls. The defaults enable classification and embedding only, so a stock deployment classifies and embeds controls but produces **no mappings**: mappings come from the relationship analyzer, which is off until you set `analysis.relationship.enabled: true`, its `models`, and at least one enabled `analysis.candidates.generators` entry. The compose stack's `deploy/config.yaml` turns mapping on: it enables `relationship` (3 samples per pair) and `requires` on the gateway's `default` alias, with the `semantic` generator (see [deploy/README.md](deploy/README.md#what-an-analysis-runs-and-what-it-costs)). `crosscodex run status <job-id>` lists the analyzers a job runs and says when it has no relationship analyzer.

The configuration does not load if an enabled analyzer cannot run: embedding without classification, requires or relationship without embedding, or requires or relationship with no enabled candidate generator (it would analyse zero pairs). Each error names the setting to change.

A job runs the analyzers enabled when it was created. If they change before the job is resumed (after a daemon restart), the job fails before any analyzer runs, and no stage is left running. The error names each analyzer whose setting differs from when the job was created. A failed job is not resumed, so start a new job with `crosscodex run start --source <catalog-id> --target <catalog-id>`; to give it the analyzers the old job had, first change the settings the error names.

### Environment Variables

The CLI recognizes these environment variables:

| Variable              | Purpose                                    | Default              |
|-----------------------|--------------------------------------------|----------------------|
| `CROSSCODEX_ENDPOINT` | daemon address                             | `localhost:50051`    |
| `CROSSCODEX_COLOR`    | Force color output (`1`) or disable (`0`)  | Auto-detect (isatty) |
| `CROSSCODEX_LOGLEVEL` | Log level (`debug`/`info`/`warn`/`error`)  | `warn`               |
| `NO_COLOR`            | Disable color output (standard convention) | —                    |

### Verbosity

Persistent flags override the configured log level for a single invocation:

- `-v`, `--verbose` — set log level to `info` (repeat `-vv` for `debug`)
- `--debug` — set log level to `debug` (implies `--verbose`; wins if combined)

Precedence (highest first): `--debug`/`-vv` → `-v` → `logging.level`
(config file or `CROSSCODEX_LOGLEVEL`) → default `warn`. The resolved level
applies to both the CLI and the auto-started embedded daemon.

## Development

### Repository Structure

CrossCodex uses a Go monorepo with separate repositories for Python ingestion and TypeScript UI:

```
crosscodex/                      # Main monorepo
  api/proto/                     # Protobuf definitions
  pkg/                           # Public SDK packages
  cmd/                           # CLI and daemon binaries
  internal/                      # Service implementations
  deploy/                        # Deployment manifests (compose stack, see deploy/README.md)
```

### Prerequisites

- **Go >= 1.26** -- see `go.mod` for exact version
- **Task** ([taskfile.dev](https://taskfile.dev)) -- install via `go install github.com/go-task/task/v3/cmd/task@latest`
- **Buf** ([buf.build](https://buf.build)) -- for protobuf code generation
- **Container engine** (podman or docker) -- for integration tests only

### Build Commands

```bash
# Build all binaries
task build

# Run all tests
task test

# Run unit tests only
task test:unit

# Lint
task lint

# Generate protobuf code
task generate
```

Run `task --list` for all available commands including integration tests and development utilities.

### Testing Strategy

| Test Type       | Framework               | Status                                  |
|-----------------|-------------------------|-----------------------------------------|
| **Unit**        | Ginkgo/Gomega (BDD)     | Available (`task test:unit`)            |
| **Integration** | Go testing + containers | Available (`task test:integration:all`) |
| **E2E**         | Venom                   | Available (`task test:e2e`)             |
| **Fuzz**        | Go native fuzzing       | Available (`task test:fuzz`)            |

#### Fuzz Tests

`task test:fuzz` fuzzes every `Fuzz*` target for `FUZZ_TIME` each (default `10s`). It runs one worker per CPU core, so expect it to occupy the machine. Narrow it with `PKGS`, for example `task test:fuzz PKGS=./pkg/backup FUZZ_TIME=1m`.

`task test` already replays every target's seed corpus and committed `testdata/fuzz/` inputs, so known crashers are regression-tested on every run. CI does not fuzz pull requests or pushes, because fuzzing takes too long for a PR workflow. The nightly `Fuzz (nightly)` workflow fuzzes every target for 2m each, uploads any new crash inputs as the `fuzz-crashers` artifact, and (scheduled runs only) opens or comments on a "Nightly fuzz run failed" issue, which is closed by hand once a run passes. GitHub disables scheduled workflows in public repositories after 60 days without repository activity; re-enable it from the Actions tab.

#### Stress Tests (#112)

Gateway stress tests verify concurrent upload handling, resource limits, data integrity, and throughput under load.

```bash
# Run in-process stress tests
task test:stress        # alias for test:stress:unit
task test:stress:unit

# Run throughput benchmark
task test:stress:bench

# Run full-stack round-trip integration suite
task test:stress:e2e
```

**Build tag:** Stress tests compile only under `-tags stress` and are excluded from the default `task test` / `go build ./...` path.

**E2E requirements:** `task test:stress:e2e` requires a podman runtime (rootless mode). The taskfile sets `TMPDIR=/workspace/.test-output/buildtmp` for build scratch. The container storage driver is taken from your podman configuration (`storage.conf`), matching the rest of the integration suite; if your environment has no overlay support, select vfs via `storage.conf` or `STORAGE_DRIVER=vfs` rather than relying on the taskfile.

#### Embedded E2E Tests (#114)

Embedded daemon end-to-end tests exercise startup (PKI, migrations, tenant provisioning, mTLS server), a full catalog round-trip over the mTLS gateway, graceful shutdown, and error paths.

```bash
# Run the embedded daemon e2e suite (starts a PostgreSQL container)
task test:integration:embedded
```

**Build tag:** Embedded e2e tests compile only under `-tags integration` and require `TEST_DATABASE_DSN`; the suite skips cleanly when it is unset. The podman `TMPDIR` / storage-driver notes under **Stress Tests** apply here too (environment-only, not baked into the Taskfile).

#### Venom E2E Tests (#18)

Venom E2E suites exercise the `crosscodexd` daemon as a black box: they start the daemon and its PostgreSQL container, drive inline OSCAL through the mTLS gateway, and assert the resulting AGE graph.

```bash
# Run the hermetic E2E suite (Tier A, no LLM required)
task test:e2e            # alias for test:e2e:venom
task test:e2e:venom

# Run the opt-in suite that adds real LLM analysis (Tier B)
task test:e2e:venom:llm
```

**Tiers:** `test:e2e:venom` (Tier A) is hermetic and asserts the OSCAL-to-AGE structural surface without an LLM. `test:e2e:venom:llm` (Tier B) is opt-in and additionally spins up Ollama and LiteLLM to exercise real LLM analysis.

**Requirements:** both suites need `venom`, `psql`, and `pg_isready` on the host plus a podman (or docker) runtime; the LLM tier also needs `jq`. The podman `TMPDIR` / storage-driver notes under **Stress Tests** apply here too.

#### Tooling Tests

Venom suites in `test/tooling/` check the repo's own scripts and Taskfile behaviour: the e2e daemon is stopped before its database is torn down, and the deploy tasks fail with an actionable message when `deploy/.env` is missing. They need only `venom` and `task`, start no containers, and never touch your `deploy/.env`. CI runs them in the `e2e-venom` job.

```bash
task test:tooling
```

### Contributing

See [CONTRIBUTING.md](./CONTRIBUTING.md) for development workflow, PR process, and coding standards.

## CI Security

All GitHub Actions workflows follow least-privilege principles:

- Default to no permissions (`permissions: {}`) with explicit per-job grants
- MegaLinter runs actionlint and other YAML-aware linters for workflow validation

## Security & Compliance

### Multi-tenant Isolation (Defense-in-Depth)

Every layer enforces tenant isolation independently:

| Layer            | Mechanism                                    | Purpose               |
|------------------|----------------------------------------------|-----------------------|
| **Gateway**      | mTLS client certificates, JWT sessions, RBAC | Identity verification |
| **Services**     | Request metadata validation                  | Context propagation   |
| **NATS**         | Tenant-scoped subjects and ACLs              | Message isolation     |
| **PostgreSQL**   | Row-Level Security policies                  | Data isolation        |
| **Object Store** | Tenant-prefixed paths, bucket policies       | Artifact isolation    |
| **Graph (AGE)**  | Separate graph per tenant                    | Traversal isolation   |

### Authentication Methods

| Method                | Use Case                            | How It Works                            |
|-----------------------|-------------------------------------|-----------------------------------------|
| **X.509 (mTLS)**      | CLI, service-to-service, automation | Client certificate during TLS handshake |
| **GSSAPI (Kerberos)** | Enterprise SSO, Active Directory    | Kerberos ticket via SPNEGO              |
| **SAML**              | Web UI, browser SSO                 | SAML assertion from IdP                 |

### FIPS 140 Support

CrossCodex supports dual builds (standard and FIPS) from the same source:

- **FIPS build**: BoringCrypto, approved cipher suites only (container image planned — Red Hat UBI base)
- **Standard build**: Go stdlib crypto (container image planned — distroless base)
- **Runtime enforcement**: `tls.fips.enabled: true` validates FIPS compliance

### Cryptographic Attestation

When attestation is enabled for a job's tenant (`attestation.enabled`, `attestation.tenant_overrides`), the pipeline records an in-toto attestation of the job in object storage under `attestations/<tenant>/<job-id>/`:

- **Layout** (`layout.json`): signed before any analyzer runs; one step per enabled analyzer, then `synthesis` and `graph`.
- **Links** (`links/<step>.json`): one signed link per step. Every link records the job, `job:<tenant>/<job-id>`, with the SHA-256 of the job's stored config, and every layout step expects it, so an attestation cannot be passed off as another job's. An analyzer's link records the SHA-256 of its stored result and of the results it consumed; synthesis records every analyzer's result and its own output (the SHA-256 of the JSON of the result it computed, not of rows an auditor can read back from the database); graph records the synthesis output. `candidate_generation` has no link.
- **Bundle** (`bundle.json`): the layout and every link, stored only after the chain verifies. The job is `COMPLETED` only once it is stored.

The job fails instead of completing unattested. Every error names the step and object:

| Failure | Job | What to do |
|---|---|---|
| Layout or a link cannot be signed | Fails (the step's stage too, for synthesis and graph); a layout failure fails it before any analyzer runs | Fix the key pair and restart `crosscodexd`, then `crosscodex run retry --from-failure <job-id>` or start a new job. If you fixed it by rotating to a new key pair, start a new job: the links the job already stored were signed with the old key and no longer verify, so a retry fails |
| Object storage refuses a write after `attestation.storage_retries` retries | Fails | Fix storage, then `crosscodex run retry --from-failure <job-id>` or start a new job |
| A stored link is missing, does not verify, or is for another job on resume, or the chain does not verify | Fails, with every stored object left as it was; `run retry --from-failure` refuses it | Start a new job; check whether objects under `attestations/` were changed |

Publishing the bundle to the audit stream is best effort: a failure is logged, since the stored bundle is the record. When attestation is disabled, no signer is built, no attestation object is written, and the daemon logs at startup that jobs are unattested.

To verify a job offline, copy its `bundle.json` out of object storage and run:

```sh
crosscodex results verify bundle.json --public-key deploy/certs/attestation-pub.pem --tenant <tenant> --job-id <job-id>
```

It checks that the bundle is bound to that job, the layout's signature and expiry, every link's signature, one link per layout step, and every digest one step passes to another, and prints the job's config digest the links record. It does not read the database.

See [Cryptographic Attestation Guide](docs/dev/attestation.md) for the attestation model, trace correlation, and how the pipeline builds the chain.

## Storage Architecture

PostgreSQL with extensions handles all data:

| Store          | Extension  | Purpose                                                     |
|----------------|------------|-------------------------------------------------------------|
| **Relational** | PostgreSQL | Job metadata, catalogs, classifications, tenant config      |
| **Graph**      | Apache AGE | Relationship graph, openCypher queries, temporal attributes |
| **Vector**     | pgvector   | Embedding similarity search                                 |

Additional storage:

| Store            | Technology     | Purpose                                                |
|------------------|----------------|--------------------------------------------------------|
| **Object Store** | Local FS / S3  | Documents, embeddings, attestation bundles             |
| **Message Bus**  | NATS JetStream | Audit trails, work distribution, service communication |

## Observability

### OpenTelemetry Integration

Built-in observability with OTLP export:

- **Traces**: Span per stage, span per LLM call, cross-service correlation
- **Metrics**: Job duration, LLM latency, worker utilization, queue depth
- **Logs**: Structured logging correlated to trace IDs. `crosscodexd` writes logs to stderr at `logging.level` (default `warn`) in `logging.format` (`text` or `json`, default `text`)

See [Telemetry Guide](docs/dev/telemetry.md) for configuration, Jaeger setup, metrics reference, and instrumentation status.

### Audit Trails

JetStream provides persistent audit streams:

| Stream        | Retention  | Content                                 |
|---------------|------------|-----------------------------------------|
| **Decisions** | Indefinite | Final compliance determinations         |
| **LLM Calls** | 90 days    | Full prompts, responses, model versions |
| **Events**    | 30 days    | Pipeline lifecycle, debugging           |

See [Audit Streams Guide](docs/dev/audit-streams.md) for provenance headers, message inspection, and trace correlation.

## Data Retention & Legal Holds

CrossCodex enforces per-data-class retention policy: expired data is archived to
a secondary store and then purged from the primary store, unless a **legal hold**
covers it. Every operation is scoped to a single tenant. Completed jobs and
catalogs are purged as aggregates with their dependents: an expired catalog is
archived as `{catalog, classifications, controls}` and then purged together with
its `classifications`, `controls`, and `embeddings`. The archive deliberately
omits `embeddings` (derived/regenerable, so purge-only); their lifetime is bound
to the parent catalog's retention rather than an independent
`retention.tiers.embeddings` tier. For how the engine works internally, see the
[Retention Lifecycle](docs/dev/retention.md) guide.

### Client CLI (`crosscodex admin`)

All admin commands take `--tenant`, which must match the tenant the caller is
authenticated for; the server authorizes every request against the caller's
tenant.

```sh
# Scan and enforce the retention lifecycle for a tenant.
crosscodex admin retention scan --tenant acme

# Preview without mutating any data.
crosscodex admin retention scan --tenant acme --dry-run

# Show retention-eligible counts and the active-hold total.
crosscodex admin retention stats --tenant acme
```

### Legal Hold Lifecycle

A legal hold prevents matching data from being purged for as long as it is
active. An empty scope field is a wildcard (matches everything), so a hold with
no `--job`/`--created-before` covers the whole tenant.

```sh
# Create a hold. --name is required. The server forces the hold's tenant to the
# caller's tenant, so a hold can never be planted for another tenant.
crosscodex admin hold create --tenant acme --name "litigation-2026-014"

# Narrow a hold to one job and/or only data created before a cutoff (RFC3339),
# and optionally auto-release it at a future timestamp.
crosscodex admin hold create --tenant acme --name "audit-hold" \
  --job job-abc123 \
  --created-before 2026-01-01T00:00:00Z \
  --expires-at 2026-12-31T00:00:00Z

# List active holds (alias: ls).
crosscodex admin hold list --tenant acme

# Release a hold by name. Released holds remain visible in `list` as an audit
# trail but no longer block purges.
crosscodex admin hold release --tenant acme --name "litigation-2026-014"
```

### Database Migration (`crosscodexd migrate`)

`crosscodexd` never migrates. Before the first start, and after every upgrade, run:

```sh
crosscodexd migrate
```

It connects as the schema owner (`database.migration_dsn`, env `CROSSCODEX_DATABASE_MIGRATION_DSN`), applies pending migrations, sets the passwords of `app_user`, `graph_user`, `purge_user`, `tenant_admin` and `backup_user` from the passwords in `database.dsn`, `database.graph_dsn`, `database.purge_dsn`, `database.tenant_admin_dsn` and `backup.dsn`, and provisions `tenants.default_tenant`. It is idempotent. The daemon and the `crosscodexd admin` commands that open the application database (`retention scan`, `reconcile artifacts`, and `adjudicate artifacts` including `review`, `show` and `decide`) refuse a superuser, `BYPASSRLS` or table-owner connection, and a schema that is not at the latest migration. `admin tenant`, `admin identity` (`database.tenant_admin_dsn`) and `admin backup` (`backup.dsn`) do not run these checks. See [Database Migrations](docs/dev/migrations.md). The embedded `crosscodex` CLI is unchanged: it migrates and provisions with the DSN it is given.

### Daemon One-Shot (`crosscodexd admin`)

For on-host operation (e.g. an external scheduler running next to the daemon),
`crosscodexd` exposes in-process one-shot commands. Like `healthcheck` and
`version`, they are **unauthenticated by design**. They run locally against the
daemon's configured resources and select their tenant with `--tenant`, or run
for every active tenant with `--all-tenants` (`retention scan --all-tenants`
covers every tenant, deleted and suspended included, because a deleted tenant's
job data still follows retention):

```sh
crosscodexd admin retention scan --tenant acme
crosscodexd admin retention scan --tenant acme --dry-run
crosscodexd admin retention scan --all-tenants

crosscodexd admin reconcile artifacts --tenant acme
crosscodexd admin reconcile artifacts --tenant acme --dry-run --max-edges 10000
crosscodexd admin reconcile artifacts --all-tenants --dry-run

crosscodexd admin adjudicate artifacts --tenant acme
crosscodexd admin adjudicate artifacts --tenant acme --dry-run
crosscodexd admin adjudicate artifacts --tenant acme --reproject
crosscodexd admin adjudicate artifacts --all-tenants --max-pairs 500

crosscodexd admin adjudicate artifacts review --tenant acme
crosscodexd admin adjudicate artifacts show --tenant acme --pair 3f9c0a1b2d4e5f60
crosscodexd admin adjudicate artifacts decide --tenant acme --pair 3f9c0a1b2d4e5f60 --verdict rejected --reviewer jdoe --expect <token>
```

`retention scan` and an `adjudicate artifacts` run without `--dry-run` or
`--reproject` publish audit events, so they attach to the daemon's NATS server. They refuse to start
when `nats.url` is empty, because embedded mode would start a second JetStream
server on the daemon's store directory. They also refuse a server whose audit
streams do not exist yet, and never create or update a stream, so a one-shot
cannot change the daemon's stream retention. Start the daemon first.

`--all-tenants` and `--tenant` are mutually exclusive, and `adjudicate
artifacts review` and `decide` take `--tenant` only. With `--all-tenants`:

- **Tenant list:** the command lists tenants through
  [`database.tenant_admin_dsn`](#tenant-administration) and skips suspended ones.
  It refuses to run when that DSN is unset.
- **Order and output:** tenants run one at a time in ID order, on one set of
  connections opened for the whole run. Each report on stdout is preceded by a
  `tenant: <id>` line.
- **Failures:** a failing tenant does not stop the rest. Its error goes to
  stderr, and once every tenant has run the command prints
  `<n> of <m> tenants failed: <ids>` and exits `1`. If the command's
  connections or configuration cannot be opened at all (for example the
  database, graph or NATS is unreachable, or the adjudicator's prompts or LLM
  client cannot be set up), the command prints that one error and exits `1`
  without running any tenant.
- **No active tenants** (`retention scan`: no tenants): the command says so on stderr and exits `0`.

`reconcile artifacts` links equivalent artifacts across controls and catalogs:

- **Exact duplicates:** artifacts with the same type and normalized name join one
  `ArtifactGroup` node through `MEMBER_OF` edges.
- **Overlapping names:** groups of one type whose names overlap (token-set overlap of
  0.6 or more) get a scored `SAME_AS` edge.

It never merges or deletes nodes. Every run is a full pass, and re-running it over
an unchanged graph writes no graph elements (it re-queues open pairs for
adjudication, which leaves already-queued pairs unchanged), so it is safe to
schedule (cron, Kubernetes CronJob) once per tenant or once with `--all-tenants`.

- **Overlapping runs:** do not run two reconciles for the same tenant at the same
  time.
- **`--max-edges`:** defaults to 50000. A run that would add more `SAME_AS` edges
  fails before writing anything; this usually means an over-broad artifact name.
- **Exit codes:** 0 on success, 1 on failure, 2 on usage errors.
- **`--dry-run`:** the `new_*` counts report what the run would have written.

`reconcile artifacts` also queues every overlapping-name pair for
adjudication in PostgreSQL (`artifact_pair_verdicts`). It skips pairs that are
closed, meaning confirmed or rejected, and reports them in its `closed` row. It
therefore needs the database as well as the graph.

`adjudicate artifacts` asks an LLM panel whether each queued pair really
denotes one artifact. Token overlap over-matches containment: "Policy" scores
1.0 against "Password Policy".

- **Confirmed:** the panel says yes. The scored edge is superseded by a
  `SAME_AS` edge with `determination_type` `llm_panel`.
- **Rejected:** the panel says no. The scored edge is superseded and nothing
  replaces it.
- **Undecided:** the panel split or fell below
  `analysis.artifact_adjudication.consensus_threshold`. Automation does not
  retry the pair; only a [human verdict](#human-verdicts) can change it.
- **Failed:** too many calls failed or replies did not parse. The pair is
  retried after a backoff (`retry_base_delay`, doubling per failure up to
  `retry_max_delay`; 1 minute and 24 hours by default) and is abandoned after
  `max_attempts` failures.
- **LLM unavailable:** if every call for a pair fails because the LLM gateway
  is unavailable or not configured, the rate limit is hit, the credentials
  cannot be resolved or are rejected, or the model is not allowed, the
  tenant's run stops, the command exits `1`, and the pair is
  not charged a failure. An outage or a wrong key therefore cannot abandon the
  queue; fix the cause and run the command again. With `--all-tenants`, an
  outage stops each tenant's run in turn: every tenant fails fast on its first
  pair, no attempts are charged, and the command exits `1` naming them.
- **Human verdicts win:** automation never overwrites a pair a human decided
  with `decide` (see [Human verdicts](#human-verdicts)). If one lands while
  the panel is running, the panel's opinion is stored as dissent and the
  graph keeps the human verdict.

Other behavior:

- **Scheduling:** run `reconcile artifacts` first, then `adjudicate
  artifacts`, each on its own schedule. Concurrent adjudicators are safe:
  each pair is leased to one run at a time, and a pair whose lease expired
  before its panel started is skipped and retried by a later run.
- **`--max-pairs`:** the most pairs sent to the panel per tenant per run
  (default 200), which bounds LLM spend. Every pair costs
  `len(models) × samples_per_model` completions.
- **Report:** `reset` (with `--reproject`: verdicts marked for
  re-projection), `recovered` (verdicts from earlier runs written to the graph
  now), `orphaned` (confirmations whose groups are gone from the graph),
  `leased`, `confirmed`, `rejected`, `undecided`, `failed`, `abandoned`,
  `dissent` (a human decided the pair meanwhile), `stale` (another run decided
  it meanwhile), `expired` (lease expired before the panel started), `fenced`
  (a newer verdict replaced the one being written to the graph, or another
  run took over the write; the newer verdict is written next) and `busy`
  (another run is writing the pair's verdict to the graph and finishes it).
- **Audit:** every panel call publishes an LLM audit event to the
  `AUDIT_LLM` stream (see [Audit Trails](#audit-trails)), so a real run also
  attaches to the daemon's NATS server (see above). A tenant run's events
  share the job ID
  `artifact-adjudication-<uuid>`, logged as `run.id`.
- **`--dry-run`:** reports `pending`, `due` and `unprojected` counts without
  leasing, calling the LLM or writing verdicts or graph elements. It opens no
  LLM client and no NATS connection, so it runs on a host without an LLM
  gateway or NATS (the configuration check below still applies). Like every
  command that opens the database, it does not migrate: it refuses an
  outdated schema and points at `crosscodexd migrate`.
- **`--reproject`:** with `--tenant` only. Marks the tenant's projected
  confirmed and rejected verdicts, LLM-panel and human alike, unprojected and
  writes them back to the graph (`reset` counts them), then stops: it leases
  no pairs and makes no LLM calls, so like `--dry-run` it needs no LLM
  gateway or NATS. Use it after rebuilding the tenant's graph, then run
  without `--reproject` to adjudicate due pairs. It cannot be combined with
  `--dry-run`, `--max-pairs` or `--all-tenants`.
- **Configuration:** the command refuses to run until
  `analysis.artifact_adjudication.enabled` is `true` and `models` is set (see
  [Key Configuration Examples](#key-configuration-examples)). `review` and
  `decide` do not.
- **Prompt:** the panel uses the `artifact_same_as` prompt. Like any other
  prompt, a YAML file with that `name` in `$XDG_DATA_HOME/crosscodex/prompts/`
  or in a directory listed in `prompt.layer_paths` overrides it. Artifact
  names and samples come from analyzed catalogs, so they are flattened to one
  line, stripped of invisible characters, capped and escaped before they reach
  the prompt.
- **Exit codes:** 0 on success, 1 on failure, 2 on usage errors.
- **Clocks:** leases, and the claims that reserve a verdict while it is
  written to the graph, are timed by the clock of the host running the
  command (`adjudicate` or `decide`). Keep every such host in sync (NTP);
  otherwise a skewed host can take a pair whose lease or claim has not yet
  expired. A host whose clock runs far ahead holds its claims that far into
  the future: until then every run skips the pair (`--dry-run` still counts
  it as `unprojected`) and its verdict does not reach the graph.
  `--reproject` does not clear a claim; clearing one early takes a manual
  database update of that pair's row.

After rebuilding a tenant's graph, re-run the reconciler, then run
`crosscodexd admin adjudicate artifacts --tenant <tenant> --reproject` to write
the tenant's verdicts back to the graph, human verdicts included.

`crosscodexd admin backup` captures, lists, verifies and restores PostgreSQL (WAL-G base backups plus WAL archiving for PITR), tenant object stores, and the JetStream audit streams:

```sh
crosscodexd admin backup run
crosscodexd admin backup list
crosscodexd admin backup verify [--point ID]
crosscodexd admin backup restore --point ID
```

- `backup run` — captures Postgres, every tenant's objects and the audit streams into a new backup point.
- `backup list` — lists complete backup points (and incomplete ones separately).
- `backup verify` — checks blob/snapshot checksums, WAL-G base backup presence, WAL continuity and staleness.
- `backup restore` — restores objects and audit streams from a backup point; Postgres is restored separately with `crosscodex-db-restore`.

Exit codes: 0 ok, 1 failure or stale, 2 usage error. See [deploy/README.md, Backups](deploy/README.md#backups) for setup, the run/verify/restore procedures, and PITR.

#### Human verdicts

`adjudicate artifacts review`, `show` and `decide` let a person settle the
`SAME_AS` pairs the panel could not, or overrule a panel verdict. The workflow
has three steps: list, read, decide.

```sh
crosscodexd admin adjudicate artifacts review --tenant acme --limit 20
crosscodexd admin adjudicate artifacts show --tenant acme --pair 3f9c0a1b2d4e5f60
crosscodexd admin adjudicate artifacts decide --tenant acme --pair 3f9c0a1b2d4e5f60 --verdict confirmed --reviewer jdoe --expect <token>
```

**1. `review`** lists up to `--limit` pairs (default 100), oldest first, that
need a person:

- `undecided` and `abandoned` pairs;
- human verdicts that a later panel run disagreed with (its opinion is stored
  as dissent).

Its columns are `PAIR` (the pair ref), `STATUS`, `BY` (who decided it:
`llm_panel`, `human`, or `token_overlap` for an abandoned pair), `VOTES`,
`TYPE`, `LOW` and `HIGH` (the two artifact groups' names), `SCORE` (token
overlap) and `DISSENT` (the dissenting verdict, or `-`). `VOTES` is the
current panel verdict's split:

- `2/3 same`: two of three valid votes said the groups are the same;
- `2/3 same (1 failed)`: the same, with one more vote that failed and did not
  count;
- `unreadable`: the stored votes cannot be decoded; `show` prints the error;
- `-`: no panel verdict (an abandoned or human-decided pair).

Names come from analyzed catalogs, so they are flattened to one line,
stripped of control and invisible characters and capped before they are
printed. With nothing to review it prints `no pairs need review`.

**2. `show`** prints everything a reviewer needs to decide one pair, so the
decision rests on the panel's reasoning and not only its vote count:

```text
PAIR        3f9c0a1b2d4e5f60
TYPE        policy
LOW         Access Control Policy
HIGH        Access Policy
SCORE       0.71
STATUS      undecided
BY          llm_panel / artifact-adjudicator
CONFIDENCE  0.50
DECIDED     2026-10-13T09:14:02.51Z
PROMPT      artifact_same_as v1.1.0

VOTES (1 same, 1 distinct, 1 failed; 2 valid of 3)
  voter-a  same  high
    Both titles name the organization's access control policy ...
  voter-b  distinct  medium
    The second document scopes only remote access ...
  voter-c  failed  -

PANEL DISSENT
  none

RELATED PAIRS (showing 1 of 1 that share a group)
  7d21e0c4b9a85f36  confirmed  llm_panel  Access Policy | Access Control Standard

EXPECT    9be41c07d3a2f685
To decide: crosscodexd admin adjudicate artifacts decide --tenant acme --pair 3f9c0a1b2d4e5f60 --verdict <confirmed|rejected> --reviewer <id> --expect 9be41c07d3a2f685
```

The example is illustrative; values vary. Its sections:

- **Header:** the pair, its artifact groups, the token-overlap `SCORE`, the
  current `STATUS`, who decided it (`BY` is determination type / decider),
  `CONFIDENCE`, `DECIDED` time and the prompt that produced the panel verdict.
- **`VOTES`:** the split, then each vote's voter, decision (`same`,
  `distinct` or `failed`), confidence and justification.
- **`PANEL DISSENT`:** on a human verdict, the panel's opinion, labelled
  `overridden by the human verdict` (the panel verdict the person replaced),
  `given after the human verdict (disputed)` (a later panel run disagreed;
  the pair is in `review`) or `given after the human verdict (agrees)`. A
  dissent that cannot be decoded is labelled `unreadable`. Otherwise `none`.
- **`LAST ERROR`:** shown only when the last panel attempt failed.
- **`RELATED PAIRS`:** other pairs that share a group with this one, with
  their status and decider. `--related <n>` sets how many to list (default
  20, must be positive); the heading says how many exist in total.
- **`EXPECT`:** the token `decide` needs, and a ready-to-edit `decide`
  command.

Everything printed from stored data is cleaned the same way as `review`.
`show` changes nothing. If the stored votes cannot be decoded, it prints the
pair with an `unreadable` `VOTES` section and the `EXPECT` token anyway, then
exits 1 with the error; the pair can still be decided.

**3. `decide`** records the verdict, using the `EXPECT` token from `show`
(see the `--expect` item below).

To change a verdict the panel already reached, find the pair with `--name`:

```sh
crosscodexd admin adjudicate artifacts review --tenant acme --name "access policy"
crosscodexd admin adjudicate artifacts decide --tenant acme --pair 7d21e0c4b9a85f36 --verdict rejected --reviewer jdoe --expect <token>
```

`review --name <text>` lists, instead of the review queue, up to `--limit`
pairs in any status (pending, confirmed, rejected, undecided, abandoned),
whoever decided them, whose `LOW` or `HIGH` name contains the text. Matching
ignores case, and `%`, `_` and `\` match only themselves. The text must be
1–200 bytes of plain text (no control or invisible characters). Same columns,
oldest first; on a pair a person decided, `DISSENT` shows the panel's
opinion, such as the verdict the person overrode. With no match it prints `no pairs match "<text>"`.

`decide` records `--verdict confirmed` or `--verdict rejected` on one pair:

- **Pair ref:** `--pair` is the `PAIR` column of `review`, 16 lowercase hex
  characters derived from the pair's internal key. It is opaque and safe to
  paste; the key itself embeds artifact names and is never printed. A ref
  that matches no pair of the tenant records nothing. `review` lists only
  pairs that need a person; use `review --name` to find the ref of any
  other pair, such as one the panel confirmed or rejected.
- **`--expect`:** required. The `EXPECT` value `show` printed for this pair.
  It identifies the state you read: the verdict, who decided it, its
  confidence, decision time, score, votes and dissent. If another person or
  a panel run changed the pair since, `decide` records nothing and says `pair
  <ref> changed since you viewed it: it is now ...`. Run `show` again, read
  the new reasoning, and decide with the new token. Queue bookkeeping
  (retries, leases, projection) does not change the token. Repeating a
  verdict the pair already holds from a person is accepted with any valid
  token. Without `--expect`, `decide` exits 2 and names the `show` command
  to run.
- **`--reviewer`:** required. The ID of the person recording the verdict,
  stored as `determined_by` on the verdict and on a confirmation's graph
  edge (at most 256 bytes, no control or invisible characters). It must be
  a registered, non-deleted reviewer of the tenant (`crosscodexd admin
  reviewer add`, see [Tenant administration](#tenant-administration));
  otherwise `decide` records nothing and names the command to run. It is not
  authenticated: like every `crosscodexd admin` command, `decide` trusts
  whoever can run it on the host.
- **Authority:** a human verdict outranks the panel, and automation never
  changes it. Any pair can be decided, including one the panel decided or
  another person decided; a panel verdict it overrides is kept as dissent.
  Repeating a pair's current human verdict changes nothing and prints
  `unchanged` (the original `determined_by` is kept), unless a later panel
  run disagreed with it (the pair is in `review`). Then `decide` with the same
  verdict re-affirms it: it records `--reviewer` and the new decision time,
  writes a fresh graph edge for a confirmation (the earlier human edge ends
  at the new decision time), prints `re-affirmed over the panel's later
  dissent`, and the pair leaves `review`. The panel's opinion stays as
  dissent.
- **Graph:** `decide` writes the verdict to the graph at once. A confirmation
  adds a `SAME_AS` edge with `determination_type` `human` and confidence 1;
  either verdict supersedes every other current `SAME_AS` edge between the
  two groups (the token-overlap candidate, a panel edge, an earlier human
  edge). It then prints the projection report: `recovered` (verdicts written
  to the graph), `orphaned` (confirmations whose groups are gone from the
  graph), `fenced` and `busy` (as for `adjudicate artifacts`).
- **Delays:** if an `adjudicate` run is writing the pair's previous verdict
  to the graph at that moment, `decide` records the verdict, prints that it
  reaches the graph when that run finishes or by re-running `decide` after a
  printed time, and leaves the pair to that run. If its own graph write
  fails, it says the verdict is recorded and when to re-run (up to
  `lease_duration` later). Re-running `decide` with the same arguments never
  records the verdict twice; it finishes the graph write.
- **Requirements:** neither command needs
  `analysis.artifact_adjudication.enabled`, an LLM gateway or NATS; they open
  only the database and the graph. `lease_duration` bounds how long one
  write may hold a pair, so it is validated (must be positive) even when
  adjudication is disabled, and a non-positive value fails config load.
- **Rebuilds:** `adjudicate artifacts --reproject` writes human verdicts back
  to a rebuilt graph along with the panel's.
- **Exit codes:** 0 on success, 1 on failure, 2 on usage errors.

#### Tenant administration

```sh
crosscodexd admin tenant create  --tenant acme --display-name "Acme Corp"
crosscodexd admin tenant list
crosscodexd admin tenant inspect --tenant acme
crosscodexd admin tenant suspend --tenant acme
crosscodexd admin tenant resume  --tenant acme
crosscodexd admin tenant import  --file tenants.yaml
```

These commands connect as the least-privilege `tenant_admin` role, never the
app or owner credential. Set `database.tenant_admin_dsn` (or
`CROSSCODEX_DATABASE_TENANT_ADMIN_DSN`) to a DSN for that role.
`crosscodexd migrate` creates the role; set its password yourself. The commands
refuse to run while the DSN is empty.

- **`create`:** creates the tenant and its graph, or renames an existing one
  without changing its status. Prints `created` or `updated`. Display names
  must not be blank or contain control characters.
- **`suspend` / `resume`:** while suspended, the gateway refuses every request
  from the tenant except health checks, until `resume`. Queued pipeline work
  still drains. Prints `suspended` or `already suspended`, and `resumed` or
  `already active`. Both refuse a deleted tenant; run `tenant restore` first.
- **`import`:** applies a YAML file in one transaction, so either every tenant
  is created or renamed, or none is. Renaming a deleted tenant is refused,
  as with `create`:

  ```yaml
  tenants:
    - tenant_id: acme
      display_name: Acme Corp
  ```

Tenants and reviewers are never removed, because their judgements are
permanent. They are soft-deleted, and a deleted one can be hidden behind a
pseudonym:

```sh
crosscodexd admin tenant delete   --tenant acme --by alice --reason "contract ended"
crosscodexd admin tenant restore  --tenant acme --by alice --reason "contract renewed"
crosscodexd admin tenant hide     --tenant acme --by alice --reason "customer requested"
crosscodexd admin tenant unhide   --tenant acme --by alice --reason "audit follow-up"
crosscodexd admin reviewer add     --tenant acme --reviewer jdoe --by alice --reason "joined review team"
crosscodexd admin reviewer list    --tenant acme
crosscodexd admin reviewer delete  --tenant acme --reviewer jdoe --by alice --reason "left the team"
crosscodexd admin reviewer restore --tenant acme --reviewer jdoe --by alice --reason "rejoined"
crosscodexd admin reviewer hide    --tenant acme --reviewer jdoe --by alice --reason "privacy request"
crosscodexd admin reviewer unhide  --tenant acme --reviewer jdoe --by alice --reason "audit follow-up"
crosscodexd admin identity trace   --pseudonym hidden-reviewer-0123456789abcdef --by alice --reason "audit query"
crosscodexd admin identity export  --out /secure/crosscodex-audit-2026-10-22.jsonl
```

These also connect as `tenant_admin`. Every command except `reviewer list`
and `identity export` changes or reveals an identity, so it requires `--by` (who is acting, 1-256
bytes) and `--reason` (1-500 bytes), is signed with the attestation keys
(`attestation.private_key_path` and `attestation.public_key_path`) and is
recorded in the database's audit log. Without the keys or
`database.tenant_admin_dsn` the commands refuse to run and change nothing.

- **`tenant delete`:** the tenant's data and verdicts are kept, but the
  gateway and every service refuse it. Retention still applies to its job
  data. `create` cannot rename a deleted tenant.
- **`tenant restore`:** brings a deleted tenant back as `suspended`; run
  `crosscodexd admin tenant resume` to reactivate it. A hidden tenant must be
  unhidden first.
- **`reviewer add`:** registers a reviewer so `adjudicate artifacts decide`
  accepts them. Prints `added`, or `already registered`. A deleted
  reviewer is restored with `reviewer restore`, not added again.
- **`reviewer delete` / `restore`:** a deleted reviewer's verdicts stay, but
  `decide` refuses new ones from them. Restoring makes them active again.
  On a deleted tenant, `reviewer add` and `restore` are refused; `delete`,
  `hide` and `unhide` still work.
- **`hide` / `unhide`:** only a deleted tenant or reviewer can be hidden.
  `hide` prints the pseudonym (`hidden-tenant-…` or `hidden-reviewer-…`),
  which then replaces the ID in `tenant list`, `reviewer list`, `retention
  scan --all-tenants` and `adjudicate artifacts show`. The pseudonym stays
  the same across unhide and hide again. Hiding is not erasure: the real ID
  remains in stored verdicts, graph edges and audit records. Tenant and
  reviewer IDs starting with `hidden-` are reserved.
- **`identity trace`:** prints the kind, tenant, identity, status and hidden
  flag behind a pseudonym. The lookup itself is recorded.
- **`identity export`:** writes the whole audit log to `--out` as JSON Lines,
  one event per line in `event_id` order, and prints `exported <n> identity
  events to <path>`. Each line has `event_id`, `occurred_at` (RFC 3339, UTC),
  `action`, `kind`, `tenant_id`, `identity_id`, `actor`, `db_user`, `reason`,
  `request` and `attestation` (base64 of the stored bytes, so their SHA-256
  can be recomputed) and `attestation_sha256`. The file is sensitive: it
  holds the real IDs of hidden tenants and reviewers, so store it securely.
  It is created with mode `0600` and never overwrites an existing file. It is
  written as `<path>.partial` first and moved into place only when complete;
  a failed export removes the partial file. It reads only, so it needs no
  `--by`, `--reason` or attestation keys. Run it before decommissioning a
  database (see [Decommissioning](deploy/README.md#decommissioning)).
- **Exit codes:** 0 on success, 1 on failure, 2 on usage errors.

### Configuration Reference

```yaml
retention:
  # Default retention per data class. Each value is "indefinite", "<n>y",
  # "<n>d", or any Go duration (e.g. "720h"). An empty value means "keep".
  defaults:
    attestation: indefinite
    job_results: 90d
    catalogs: 2y
    embeddings: 90d
    # Audit-class tiers are accepted for config completeness but do NOT drive
    # engine purges — the retention engine never purges audit data. Audit
    # retention is enforced by JetStream stream MaxAge via
    # nats.streams.audit_llm_retention / audit_events_retention
    # (see docs/dev/audit-streams.md).
    audit_decisions: indefinite
    audit_llm: 90d
    audit_events: 30d

  # Optional cron expression for the in-daemon scheduler. Empty = disabled
  # (drive scans externally via the one-shot instead; see below).
  scan_schedule: ""

  # Archive backend for data moved out of the primary store before purge.
  archive:
    backend: local          # local | s3 | "" (disabled)
    local:
      path: /var/lib/crosscodex/archive
    s3:
      bucket: crosscodex-archive
      storage_class: GLACIER

  # Per-tenant overrides. Non-empty fields replace the matching default;
  # empty fields fall back to the default.
  tenants:
    acme:
      job_results: 1y

database:
  # Purge runs under a dedicated, RLS-scoped purge_user role that the
  # delete-path immutability triggers accept. When purge_dsn is unset, scans
  # still run but purges fail closed rather than deleting under the wrong role.
  purge_dsn: "${PURGE_DATABASE_DSN}"   # e.g. postgres://purge_user:...@localhost:5432/crosscodex

backup:
  # postgres:// URL for backup_user (LOGIN REPLICATION).
  # Required when destination.backend is set. Never logged.
  # CROSSCODEX_BACKUP_DSN sets this field from the environment.
  dsn: ""
  destination:
    backend: ""           # local | s3 | "" (backup disabled; fails closed)
    local:
      path: ""              # absolute; must not overlap storage.objects.base_path
    s3:
      bucket: ""
      region: ""
      endpoint: ""           # S3-compatible stores; enables path-style addressing
      storage_class: ""
  max_age:
    # `admin backup verify` reports a store stale once its newest backup
    # point is older than this. Each value must be greater than zero.
    postgres: 26h
    objects: 26h
    nats: 26h
```

### External Cron Example

When `retention.scan_schedule` is left empty, drive scans from an external
scheduler using the daemon one-shot. Example crontab entry running a nightly
scan of every tenant at 02:00:

```cron
0 2 * * *  crosscodexd admin retention scan --all-tenants >> /var/log/crosscodex/retention.log 2>&1
```

To scan only one tenant, use `--tenant acme` in place of `--all-tenants`. Do not
schedule both at the same time: non-dry-run scans share one global lock, so one
of them would fail every night (see [Backups](deploy/README.md#backups)).

Artifact deduplication runs the same way. Reconcile first, then adjudicate:

```cron
0 3 * * *  crosscodexd admin reconcile artifacts --all-tenants >> /var/log/crosscodex/reconcile.log 2>&1
30 3 * * * crosscodexd admin adjudicate artifacts --all-tenants >> /var/log/crosscodex/adjudicate.log 2>&1
```

## Uninstall

To fully remove CrossCodex:

1. **Remove the binary** (location depends on your install method):
   ```sh
   rm $(which crosscodex)
   ```

2. **Remove configuration**:
   ```sh
   rm -rf "${XDG_CONFIG_HOME:-$HOME/.config}/crosscodex"
   ```

3. **Remove data** (prompt layers):
   ```sh
   rm -rf "${XDG_DATA_HOME:-$HOME/.local/share}/crosscodex"
   ```

4. **Remove state** (daemon PID, embedded TLS certificates):
   ```sh
   rm -rf "${XDG_STATE_HOME:-$HOME/.local/state}/crosscodex"
   ```

5. **Remove project-level configuration** (run in each project directory):
   ```sh
   rm -rf .crosscodex/
   ```

6. **Remove shell completions** (if installed):
   ```sh
   rm -f ~/.bash_completion.d/crosscodex
   ```

- **Issues**: [github.com/complytime-labs/crosscodex/issues](https://github.com/complytime-labs/crosscodex/issues)
- **Discussions**: [github.com/complytime-labs/crosscodex/discussions](https://github.com/complytime-labs/crosscodex/discussions)
- **License**: [Apache 2.0](./LICENSE)

### Related Projects

- [CrossCodex Ingestion](https://github.com/complytime-labs/crosscodex-ingestion) - Python document conversion service
- [CrossCodex UI](https://github.com/complytime-labs/crosscodex-ui) - React web interface
- [Docling](https://github.com/DS4SD/docling) - Document extraction library
- [Apache AGE](https://github.com/apache/age) - Graph extension for PostgreSQL
- [NATS](https://nats.io/) - Cloud native messaging system
- [in-toto](https://in-toto.io/) - Supply chain attestation framework

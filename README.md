# CrossCodex

A Go-first, multi-service compliance mapping platform that compares compliance standards, maps relationships between requirements, and stores the complete graph with full traceability.

CrossCodex delivers composable microservices, provider-agnostic LLM integration, and multi-tenant security with defense-in-depth.

______________________________________________________________________

> LLM WARNING: This project was written with LLM (AI) assistance.

______________________________________________________________________

## Status

CrossCodex is in early development. All foundational, domain, and service packages are implemented and tested. The CLI provides approximately 22 commands across project, catalog, run, results, prompt, version, and completion groups with daemon connectivity and embedded single-node mode. See [Development](#development) below to build from source and run tests.

## Upgrading

Operations progress, prompt rendering and eval tooling (#278). Additive; no breaking changes:

- **Database.** Run `crosscodexd migrate` to apply migration `004_stage_progress` (the daemon never migrates). It adds the table `job_stage_progress`, which retention archives and purges with its job. Its down migration drops the table and loses the recorded progress only.
- **`JobStage.progress`** is a new optional field of `GetJob` and of `run status --json` (`stages[].progress`), unset for a stage with no recorded progress and for every job that ran before the upgrade. `run status` prints a "Task progress" section. See [Seeing progress](#8-start-an-analysis).
- **New config field `analysis.engine.progress_flush_interval`** (default 10s) sets how often a running stage writes its progress. Nothing needs changing.
- **New RPCs `ListCatalogControls` and `RenderPrompt`**, behind `crosscodex catalog controls` and `crosscodex prompt render`. The gateway, `pipeline.proto` and the embedded daemon serve them.
- **`content_hash` is a new key** in `catalog list --json` and `catalog inspect --json`: the SHA-256 of the bytes the catalog was imported from, `""` when none was recorded. `catalog inspect` also prints it as `Content SHA-256`.
- **Embedders of `internal/pipeline`:** `pipeline.WithAnalysisConfig` is a new option. Without it `RenderPrompt` refuses every request; existing wiring is unaffected. The embedded CLI builds its prompt registry on first use rather than at startup.
- **Eval:** `task eval:crosswalk` accepts `JOB=<id>` to score an existing job. See [test/eval/crosswalk/README.md](test/eval/crosswalk/README.md#scoring-an-existing-job).

Local document ingestion overlays (#214). `task deploy:up` applies the PKI change to an existing deployment:

- **The deploy PKI gains an `ingestion` server certificate**, loaded into a new volume `crosscodex-certs-ingestion` whether or not ingestion runs. A set without it fails verification, so the first `task deploy:up`, `deploy:certs` or `deploy:local` after upgrading replaces the PKI: a new CA, client certificate and attestation keypair. Back up `deploy/certs/attestation-pub.pem` first, let running analysis jobs finish, and re-point clients at the new `ca.pem`, `client.pem` and `client-key.pem`, as for #298 below.
- **`task deploy:local INGESTION=1`** adds the ingestion service, and `INGESTION=rustfs` also moves object storage to a local RustFS bucket (needs `RUSTFS_ACCESS_KEY` and `RUSTFS_SECRET_KEY` in `deploy/.env`). `deploy:local:down` always removes the ingestion service; pass `INGESTION=rustfs` to it as well to remove RustFS. See [deploy/README.md](deploy/README.md#document-ingestion-ingestion).
- **A new `ingestion` config block** (`address`, `poll_interval`, `convert_timeout`, `max_document_bytes`; all global-only, TLS from `tls.targets.ingestion`) wires the gateway to the ingestion service. With `ingestion.address` empty, the default, nothing changes: a document that is not OSCAL JSON is still rejected. With it set, everything except OSCAL JSON goes to ingestion, including OSCAL XML and YAML, which no longer get the `oscal-cli` conversion advice. A converted document that the heading, table and numbering rules cannot split is refused with `FailedPrecondition`. See [Document ingestion](#document-ingestion).
- **`COMPOSE_OVERLAY` is a space-separated list** of overlay files; a single path works as before.

OSCAL mapping export and release readiness (#215, #17). Mostly additive; four behaviours change:

- `crosscodex results export` accepts `--format oscal-mapping`, which writes a completed job as an OSCAL 1.2.3 mapping collection. See [docs/oscal-mapping-export.md](docs/oscal-mapping-export.md).
- **`--control` is trimmed** of surrounding whitespace for `json` and `csv` exports, and for `results candidates`, so ` AC-1 ` now matches `AC-1`. Otherwise `json` and `csv` output is unchanged.
- **`task deploy:down VOLUMES=1` removes only the key volumes that exist,** each by exact name and without `-f`. A volume still in use, or a failed volume listing, now fails the task instead of being skipped.
- **The offline overlays read `CROSSCODEX_GATEWAY_CONFIG_DIR`** for the gateway config directory. It defaults to the previous path, so nothing changes unless you set it.
- **Embedded-mode `GetJob` returns per-stage panel provenance,** as the daemon does, where it returned none.

Mapping feedback and cleaning re-projection (#230, #289):

- **Database.** Run `crosscodexd migrate` to apply migration `003_mapping_feedback` (the daemon never migrates). It adds `mapping_votes`, `mapping_determinations`, `mapping_reviews` and the view `mapping_cleaning_states`, and lets findings and decisions take the status `stale`. None of these tables is purged by retention.
- **`GetReviewQueue`, `SubmitVote` and `RecordDetermination` take a required `job_id`.** `GetReviewQueue` listed no particular job and needed `admin`; it now lists one completed job's mappings and allows `reviewer` or `admin`. `RecordDetermination` is a new RPC. A request without `job_id` gets `InvalidArgument`. See [Mapping Feedback](#mapping-feedback-crosscodex-results).
- **`mapping_id` is a stable verdict key.** It names one job's judgement of one control pair and never moves: a restored mapping lives under new edge IDs, so resolve a mapping by its verdict (`job_id` and `mapping_id`), not by following edge IDs.
- **The standalone gateway and the embedded daemon serve `AdminService`** (retention, legal holds); before, only role `all` did. With `database.purge_dsn` set on the `gateway` role, purges and catalog retire work there, so the internet-facing process then holds purge credentials; leave it unset to keep them off the gateway, and purges fail closed. The embedded daemon serves holds, stats and dry-run scans only: a scan or policy apply without `dry_run` is refused with `FailedPrecondition`, and stats report no stream drift.
- **The embedded daemon's client certificate has roles `admin` and `reviewer`,** so its single local user can vote and read the review queue. The embedded daemon refuses determinations (see below).
- **New `feedback` config section** (`min_agreement`, `low_confidence`, both default 0.5, global only).
- **New `crosscodexd admin cleaning --reproject`** re-applies stored cleaning findings and determinations to a rebuilt graph. See [Re-projecting after a graph rebuild](#re-projecting-after-a-graph-rebuild).

Results show cleaning state (#291). Additive:

- **`ListJobMappings` and `results export` show what cleaning or a reviewer did to a mapping.** `ControlMapping` gained `cleaning` (state `retired`, `restored` or `stale`; source `finding` or `determination`; `ref_id`; `changed_at`; `graph_applied`), unset for a mapping nothing changed; the latest change wins (`changed_at`, then source, then `ref_id`). `results export` JSON adds a `cleaning` key only on such mappings, and CSV (both stages) appends a last `cleaning_state` column, empty otherwise, so scripts that read the analyzer-stage `critique` column as the last one must use its header name. `results debug` prints a `Cleaning:` line (`retired: finding 1a2b3c4d5e6f`, ID cut to 12 characters).

Every TLS server has its own key and certificate (#298). `task deploy:up` applies this to an existing deployment, and the first run after upgrading replaces the PKI:

- **Back up `deploy/certs/attestation-pub.pem` first**, and let running analysis jobs finish. `deploy/certs/` used to hold one `server.pem` and `server-key.pem` that `db`, `nats`, the LLM gateway and the `crosscodexd` listener shared, so any of them could present a valid certificate for any other. `deploy/certgen/gen.go --verify` now rejects that layout, and `task deploy:up` replaces `deploy/certs/` with a new CA, a new client certificate, a new attestation keypair and one key and certificate per service (`db`, `nats`, `litellm`, `praxis`, `crosscodexd`), each naming only that service.
- **Re-point clients.** Trust the new `deploy/certs/ca.pem` and present the new `client.pem` and `client-key.pem` (`--tls-ca`, `--tls-cert`, `--tls-key`, or `crosscodex config set tls.ca`, `tls.cert`, `tls.key`). The old client certificate is rejected. Attestations signed with the old key verify only against the old `attestation-pub.pem`; a job still running when the key changes fails when it resumes.
- **The server certificate volume `crosscodex-certs` is replaced by one volume per service**, `crosscodex-certs-db`, `-nats`, `-litellm`, `-praxis` and `-crosscodexd`. `CROSSCODEX_CERTS_VOLUME` is now their prefix. Each service mounts only its own, at the same in-container paths as before (`/certs/server.pem`, `/certs/server-key.pem`; `crosscodexd` at `/server-certs`), so `deploy/config.yaml` and any config that names those paths need no change. Running compose by hand needs all the volumes: `task deploy:certs` creates them. Remove the old volume, which still holds the old shared key: `podman volume rm crosscodex-certs` (or `docker volume rm`).
- **Rolling back to the previous release** needs `task deploy:down`, then `podman volume rm` of the per-service volumes (`crosscodex-certs-db` and the others above), then the previous release's `task deploy:up`, which regenerates the shared layout.
- **The integration, end-to-end and embedded PKIs no longer issue a dual-use server certificate.** `testcerts.Generate` and the embedded CLI's `pki/` server certificate permit server authentication only, and a CLI-generated `pki/` with the older certificate is regenerated on next use (a set you supplied is kept as is). The e2e daemon (`test/e2e/run-daemon.sh`) now presents `client.pem` as its own identity and `server.pem` only on its gateway listener.

Health checks follow each role's dependencies (#226):

- **`/healthz` now fails when NATS (or, for `graph` and `all`, the graph database) is down.** Each role checks only what it uses: `worker` NATS; `pipeline` database and NATS; `graph` database, NATS and graph database; `gateway` database and NATS; `all` database, NATS and graph database. The LLM is not checked. A role that used to report healthy while it had lost NATS now reports 503 and is restarted by its orchestrator.
- **Split roles on one host need distinct `health.addr` values.** Every split role defaults to `:9091`. A role whose `health.addr` is taken used to start without a health endpoint; it now refuses to start and names `health.addr`. `graph`, `pipeline` and `worker` also refuse an empty or non-`host:port` `health.addr`. Set `health.addr` (`CROSSCODEX_HEALTH_ADDR`) per role, or run each role in its own container.
- **`crosscodexd healthcheck` probes the role's own endpoint.** For `graph`, `pipeline` and `worker` it now probes `http://127.0.0.1:<health.addr port>/healthz` instead of the gateway's HTTPS `server.addr`. It reads the role from `--role`, else `role` / `CROSSCODEX_ROLE`, so a container that starts the daemon with `--role worker` must run `crosscodexd --role worker healthcheck` (or set `CROSSCODEX_ROLE`).
- **The dedicated `/healthz` body names the failing dependency, not the error.** It is `{"status":"unhealthy","error":"nats unreachable"}`; the underlying error is logged.

Compose services are hardened and limited (#236). `task deploy:up` applies these to an existing deployment:

- **Back up `deploy/certs/` first** (at least `attestation-pub.pem`). `task deploy:up` regenerates the PKI by itself when the set fails verification, for example after a release adds a SAN, and that mints a new CA and attestation keypair; attestations signed with the old key then verify only against the old `attestation-pub.pem`.
- **Memory limits.** `db` and `crosscodexd` are capped at `2g`, `nats` at `512m`, and every other service has a limit too. A service that needs more is OOM-killed and restarts in a loop without a CrossCodex error; `podman inspect --format '{{.State.OOMKilled}}' <container>` (or `docker inspect`) shows `true`. Raise a limit in `deploy/.env`: `CROSSCODEX_DB_MEM_LIMIT`, `CROSSCODEX_DAEMON_MEM_LIMIT`, `CROSSCODEX_NATS_MEM_LIMIT`, and the others in [Container hardening and resource limits](deploy/README.md#container-hardening-and-resource-limits).
- **Read-only root filesystems.** Every service except `db-restore` runs with a read-only root filesystem and a tmpfs at `/tmp` (and `/run/postgresql` for `db`). Files written to a tmpfs count against the memory limit. A compose override or custom image that writes anywhere else fails; give it a volume or tmpfs for that path.
- **Every capability is dropped and `no-new-privileges` is set**, except the capabilities `backup-init` adds back for its one-time `chown` and `chmod` of the backup volume.
- **Certificates are mounted read-only** in every service.
- **The CA private key is no longer in any certs volume.** `ca-key.pem` stays in `deploy/certs/` on the host; the next `task deploy:up` reloads the volume and removes it from an existing one.
- **The daemon's client key and the attestation keypair moved to two new volumes.** `crosscodex-client-certs` (`ca.pem`, `client.pem`, `client-key.pem`) is mounted only by `migrate` and `crosscodexd`, at `/certs`. `crosscodex-attestation-keys` (`attestation-key.pem`, `attestation-pub.pem`) is mounted only by `crosscodexd`, at `/attestation`. `crosscodex-certs` (superseded by per-service volumes in #298, see above) held only `ca.pem`, `server.pem` and `server-key.pem` for `db`, `nats` and the LLM gateway; `crosscodexd` mounted it at `/server-certs`. The next `task deploy:up` creates and loads the new volumes and removes `client.pem`, `client-key.pem`, `attestation-key.pem` and `attestation-pub.pem` from an existing `crosscodex-certs`. If you keep your own `deploy/config.yaml`, change `tls.targets.gateway-server.cert` and `.key` to `/server-certs/server.pem` and `/server-certs/server-key.pem`, and `attestation.private_key_path` and `public_key_path` to `/attestation/attestation-key.pem` and `/attestation/attestation-pub.pem`; with the old paths `crosscodexd` fails to start because the files are no longer under `/certs`. Running compose by hand needs all three volumes: `task deploy:certs` creates them. `task deploy:down VOLUMES=1` removes them.
- **The server certificate is now issued for server authentication only, so the upgrade regenerates the PKI.** `server.pem` used to permit client authentication too, and `db`, `nats` and the LLM gateway can read its key, so any of them could connect to NATS (which accepts every CA-signed client certificate) as a full client. `deploy/certgen/gen.go --verify` now rejects such a certificate, so the first `task deploy:up` of this release replaces `deploy/certs/` with a new CA, new server and client certificates and a new attestation keypair. Before upgrading, back up `deploy/certs/attestation-pub.pem` (it verifies attestations signed so far) and let running analysis jobs finish: a job still running when the attestation key changes fails when it resumes. Point the CLI at the new `client.pem` and `client-key.pem` afterwards.
- **Rotate the keys if `db`, `nats` or the LLM gateway may have been compromised.** Before this release those services could read `client-key.pem` and `attestation-key.pem` (Praxis too, if a compose tool that ignores `subpath`, such as podman-compose before 1.6.0, started it). Removing the files from the volume does not revoke copies taken earlier; the regeneration above replaces the CA, so copied client keys stop being trusted, but a copied attestation key can still sign bundles that verify against the old `attestation-pub.pem`. To rotate again later, back up `deploy/certs/attestation-pub.pem`, remove `deploy/certs/`, and run `task deploy:up` with no analysis jobs running: that mints a new CA, server and client certificates and attestation keypair and reloads every volume.
- **Rolling back to the #236 release needs the server volume removed.** That release strips `crosscodex-certs` to `ca.pem`, `server.pem` and `server-key.pem`; the previous release's `task deploy:up` treats that volume as current, skips the reload, and its `migrate` and `crosscodexd` fail to start because `/certs/client.pem` is missing. Run `task deploy:down`, then `podman volume rm crosscodex-certs` (or `docker volume rm crosscodex-certs`), then the previous release's `task deploy:up`, which reloads the volume from `deploy/certs/`.
- **Backups spool to the backup volume.** With `compose.backup.yaml`, `crosscodexd` sets `TMPDIR=/backup/tmp`, so backup files are written to disk under the backup volume rather than to `/tmp`.

Client certificates get roles only from `auth.x509_mappings` (#232):

- **Every client certificate needs a mapping; none is admin implicitly.** Roles `all` and `gateway` (and the embedded CLI daemon) used to grant `admin` to any certificate the CA signed. Now a certificate gets the roles of the first `auth.x509_mappings` entry it matches and is refused if it matches none, and the daemon refuses to start while `auth.x509_mappings` is empty. For the certificates `task deploy:certs` generates (deploy/certgen), add `auth.x509_mappings: [{match: {cn: client, organization: CrossCodex}, roles: [admin]}]`; the shipped `deploy/config.yaml` already has it. Roles must be `admin` or `reviewer`, and every match needs at least one field. The embedded CLI daemon grants `admin` and `reviewer` only to its own client certificate, matched by its exact common name and organization.
- **`tenants.enabled` is honoured.** With `false`, every identity is in `tenants.default_tenant`, and a mapping's `tenant` must be empty or that tenant. With `true`, each mapping must name a `tenant`, and a caller works in its mapping's tenant.
- **A reviewer needs a certificate of their own.** `crosscodex admin cleaning decide` needs the `reviewer` role, which `admin` does not include, so issue each reviewer a separate certificate and map it with `roles: [reviewer]`. Don't add `reviewer` to the shared admin mapping: one certificate holding both roles would let one person both apply a cleaning pass and decide its findings.
- **A split pipeline role needs `tls.service_identities`.** The standalone `pipeline` role now honors the `x-tenant-id` header only from a client certificate listed there, and refuses to start with the list empty or with a `tenants.default_tenant` that `crosscodexd migrate` has not provisioned (or that is deleted). List the gateway's outbound client certificate, e.g. `tls.service_identities: [{cn: client, organization: CrossCodex}]`; the shipped `deploy/config.yaml` already has it. That deployment uses one `client.pem` for every role, the operator and the gateway's service identity, so there the list separates nothing: anyone holding `client.pem` can assert any tenant. With `tenants.enabled: true`, give the gateway a dedicated client certificate, issued to no user and no other role, and list it alone in `tls.service_identities`. Roles `all` and the embedded CLI daemon are unaffected.

A split `gateway` reaches a standalone `graph` role (#230):

- **The `gateway` role needs `graph.endpoint`.** Role `gateway` serves graph queries, similarity search, crosswalk export and edge cleaning through a `graph` role over mutual TLS (TLS target `graph-client`: client certificate, key and CA), and refuses to start without `graph.endpoint` (`host:port`). It used to start without one and answer these RPCs with `Unavailable`. Role `all` and the embedded daemon are unaffected, except as noted below.
- **The `graph` role needs `graph.addr`, mutual TLS, `tls.service_identities`, `pipeline.endpoint` and a provisioned default tenant.** Role `graph` now also runs a Connect listener on `graph.addr` (`host:port`; TLS target `graph-server`, which must resolve to `mutual` mode with a CA). It refuses to start without `graph.addr`, mutual TLS, a non-empty `tls.service_identities` (list only the gateway role's outbound client certificate; the daemon refuses an empty list but cannot check how broad the list is), `pipeline.endpoint`, and a `tenants.default_tenant` that `crosscodexd migrate` has provisioned. The listener serves read-only graph queries, crosswalk export and edge cleaning; graph writes still go through NATS materialization. The graph role also dials the pipeline role over mutual TLS: TLS target `pipeline-client` must resolve to a client certificate, key and CA.
- **The pipeline role's `tls.service_identities` must list the graph role's client certificate.** Edge cleaning on the graph role asks the pipeline role (at `pipeline.endpoint`) which prompt and model revisions each stage would use now. If the pipeline role does not trust the graph role's certificate, or cannot be reached, cleaning fails with `Unavailable`. Through the gateway the user sees only that code and an error reference; the gateway log records the full message under that reference (it names `pipeline.endpoint` and `tls.service_identities`, which are what to check) and the graph role's log has the cause.
- **Cleaning apply and decide on the graph role need the attestation key paths.** Set `attestation.private_key_path` and `attestation.public_key_path` on the `graph` role, not on the gateway. Without both, the graph role still starts, and `run --apply` and `decide` are refused with a message naming the keys; keys that are set but do not load stop startup.
- **Role `gateway` now serves `EdgeCleaningService`.** It used to answer `Unimplemented`; it forwards to the graph role. The embedded daemon reports graph queries, crosswalks and edge cleaning as needing the crosscodexd daemon (`Unavailable`, "needs the crosscodexd daemon (role all, or gateway with a graph role)").
- **A user is always asserted with its roles.** A service identity that names an end user must send both `x-user-id` and `x-user-roles`. The pipeline and graph listeners now refuse, with `InvalidArgument`, `x-user-id` without `x-user-roles` (and the reverse), a repeated or empty `x-user-id`, a repeated `x-user-roles`, and a role list with an empty element or a role outside `admin` and `reviewer`. Before, `x-user-id` alone was accepted.
- **The gateway's clients refuse an identity with no subject.** The gateway's pipeline and graph clients fail the call with `FailedPrecondition`, without sending it, when the authenticated identity has an empty subject, instead of forwarding the tenant alone. A client certificate mapped by organization or organizational unit with no common name now fails gateway to pipeline calls in split shapes; give such certificates a CN. Role `all` calls in process and is unaffected.
- **Graph queries refuse `RETURN *`.** `GraphService.Query` now answers `RETURN *` with `InvalidArgument`; name each returned value instead (`RETURN n, m`). A `RETURN` with several items now returns one value per item in each row; before, it failed with an internal error.

Graph materialization is durable, and a job completes only once its graph is written (#231):

- **A split deployment needs a running graph role.** A job's graph stage now completes only when a `graph` role (or `all`) confirms that every materialization event of the job is written. Before, it completed as soon as the events were published, so a job could report `completed` with no graph written. Without a graph role on the pipeline's NATS server, every job fails at its graph stage after the new `pipeline.graph_materialize_timeout` (default `30m`, range `(0, pipeline.stage_timeout]`; global-only) with a message naming the timeout and the missing graph role; raise it if a large job's graph takes longer to write. A `failed` graph outcome fails the job with the graph role's reason. A retried or resumed job re-publishes the completion events of the stages an earlier run completed, so their graph is written and checked again. See [Job completion needs a graph role](docs/dev/service-runtime.md#job-completion-needs-a-graph-role).
- **Behaviour.** The graph role now reads stage completions and parsed catalogs from the `MATERIALIZE` stream through the durable consumer `graph-materializer`, instead of a core NATS queue group. An event published while no graph role runs waits for one. A failed event is retried; an edge whose control node is not projected yet is retried instead of skipped at once, and is skipped only on the last delivery. When an event fails on its last delivery, the graph role stores a `failed` outcome on `crosscodex.audit.<tenant>.events.<job>.graph.failed` (stream `AUDIT_EVENTS`). A job barrier on `crosscodex.materialize.<tenant>.<job>.barrier` (consumer `graph-barrier`) is answered with `...graph.materialized` or `...graph.failed` once none of the job's events is left in `MATERIALIZE`. An event or barrier whose subject tenant differs from its provenance tenant is dropped.
- **Config.** New global-only `nats.materialize`: `max_deliver` (default `5`, at least `1`; the graph role enforces it, not the server, so a delivery past it, such as after a lost ack, is handled as the last one), `ack_wait` (default `10m`, positive; must exceed the time to materialize one event, since a longer one is redelivered while it still runs), `redeliver_delay` (default `30s`, positive) and `barrier_poll_interval` (default `2s`, positive; how often a waiting barrier is checked). A barrier counts a `failed` outcome only if `AUDIT_EVENTS` stored it at or after the barrier's `not_before` (the run's start), so a retried job is not failed by its earlier run. `not_before` is read from the pipeline host's clock and compared with NATS server timestamps, so keep pipeline and NATS hosts NTP-synced.
- **Logging.** A durable handler that returns `natsbus.ErrSkip` (ignored message) or `natsbus.ErrNotReady` (condition not met yet; redelivered) is logged at debug, and `natsbus.ErrPermanent` terminations at warn; other handler errors stay at error.
- **Streams are verified, not overwritten.** A role creates a missing JetStream stream but never updates a live one. If a live stream's subjects, retention policy, storage, `max_age` or replicas differ from this role's `nats.streams`, startup fails with `ErrStreamDrift` naming each field with its live and configured values. Make `nats.streams` agree in every role, or run `nats stream edit <name> ...` and restart. Streams an earlier build created with different retention now need that one-time edit.
- **New stream.** New `nats.streams.materialize_retention` (default `168h`, must be positive) sets the `MATERIALIZE` work-queue stream, which is created at startup.
- **Audit records are stored or the operation fails.** An LLM call whose audit event the `AUDIT_LLM` stream does not acknowledge now fails with `ErrAuditFailed` (task result `audit_failed`, never retried: the call was made and probably billed). Catalog import returns `Unavailable` when its `catalog_parsed` audit record is not stored (the catalog stays stored; re-import is safe), and a job fails when its attestation bundle event is not stored. The catalog audit subject is now `crosscodex.audit.<tenant>.events.catalog.<catalog>.parsed` (was the unscoped `audit.catalog.parsed`, which no stream captured).

Signed record of every applied cleaning run (#290):

- **Database.** The new append-only table `edge_cleaning_runs` is folded into the baseline migration `001_initial_schema`, and `edge_finding_decisions` gains a foreign key from a policy decision to its run. A database created by an earlier build must be recreated (`task deploy:down VOLUMES=1`, start the stack, `crosscodexd migrate`); `crosscodexd` refuses to start on the old schema, reporting `edge_cleaning_runs.plan_token` as missing.
- **Behaviour.** `crosscodex admin cleaning run --apply` now records one signed run (the canonical plan, including the effective policy, and its `cleaning.run` link) whenever the plan has findings, so moves to `reported`, `pending` and `cleared` are attested too, not only retirements. A plan with no findings is still neither signed nor recorded. Runs are read from the table; no RPC returns them yet. See "Append-only runs and decisions" in [`docs/dev/cleaning.md`](docs/dev/cleaning.md).

Edge cleaning (#272):

- **Database.** `job_stages` gains the column `stage_record`, and the new tables `edge_findings` and `edge_finding_decisions` are folded into the baseline migration `001_initial_schema`. A database created by an earlier build must be recreated (`task deploy:down VOLUMES=1`, start the stack, `crosscodexd migrate`); `crosscodexd` refuses to start on the old schema and names the missing column. Re-import your catalogs and re-run your jobs.
- **New `reviewer` role.** `EdgeCleaningService.DecideFinding` (`crosscodex admin cleaning decide`) needs it; `admin` does not include it. Grant it through `auth.x509_mappings` (see the #232 entry above). `RunCleaning` needs `admin`; `ListFindings` and `GetFinding` need `reviewer` or `admin`.
- **New `cleaning` config section** (`max_retire_per_run`, `signals.<signal>.action`, `signals.low_agreement.min_agreement_fraction`, all tenant-overridable). The defaults report every signal and retire nothing. See [Edge Cleaning](#edge-cleaning-crosscodex-admin-cleaning).
- **A restored edge has a new ID.** Restoring a retired verdict creates new edges whose IDs derive from the retired edge and the restore decision; the original edge keeps its ID and stays superseded. `mapping_id` in `results` output is still derived from the job and control pair, so it names the original, superseded analyzer edge, not the restored one.
- **Signing.** `run --apply` and `decide` need `attestation.private_key_path` and `attestation.public_key_path`, even with `attestation.enabled: false`. Role `all` now loads those keys at startup whenever both are set, and refuses to start if they do not load.

Print-artefact clean-up and blank classify skips (#277):

- Embedding text no longer has US Federal Register print artefacts stripped. Before, any line holding `VerDate`, `Jkt <n>`, `PO <5 digits>`, `Frm <n>`, `Fmt <n>`, `Sfmt <n>` or `G:\COMP\` was deleted before embedding, on every catalog and on search queries, so a one-line OSCAL statement such as "see PO 12345" was embedded as empty text. Those statements now embed in full. A new analysis job recomputes the affected embeddings (their cache key covers the text); catalog search embeddings stored by an earlier build keep the old text until the catalog is re-imported.
- Print-artefact clean-up is now opt-in and applies only to controls structured from a document, through the new `catalog.structuring.print_artifact_patterns` (global-only; default `[]`, no clean-up). No publisher's layout is built in: list the running headers, footers or paths of the documents you import (see [Print-artefact patterns](#print-artefact-patterns)). Each match is removed from a structured control's title and text, except that a text the patterns would leave blank is kept whole. A pattern that is not valid regex or matches empty text fails config loading, naming the entry.
- The `classify` stage summary gains `skipped_blank_statement_count`, the part of `skipped_count` that had no statement text, and pre-built skip results carry `skip_reason` (`excluded` for a section or withdrawn control, `blank_statement` otherwise). Blank statements were already skipped without an LLM call.

Similarity search (#271):

- `SimilaritySearch` with `query_embedding` now requires `catalog_ids` and `model`, and a vector whose width differs from the stored one returns `InvalidArgument` (was `Internal`). Before, it searched a nonexistent `"generic"` catalog and returned `NotFound`.
- `FindSimilar` / `SimilaritySearch` by `control_id` now returns real matches from the control's catalog, or from `catalog_ids`. Before, it sent an empty vector, the store rejected it and the caller got `Internal`. It returns `NotFound` when the control has no stored embedding. If the control has embeddings from several models it returns `InvalidArgument` listing them; pass `model` on `SimilaritySearch` to choose one. See [docs/dev/embeddings.md](docs/dev/embeddings.md#similarity-search).
- New `graph.max_similarity_catalogs` (default 100, range 1-10000, global only) bounds the catalogs in one search.
- New `graph.max_similarity_limit` (default 1000, range 1-10000, global only) is the largest `limit` a search may ask for per catalog. A larger `limit` returns `InvalidArgument` naming the maximum; before, it was unbounded.

Catalog document versions (#204):
- Additive, nothing breaks. `catalog import` gained `--document` and `--version-label`. `JobConfig`'s `CatalogSnapshot` and `CatalogSummary` (`results export` and crosswalk `analysis`/`catalogs`) gained `version_display` and `supersedes_catalog_id`, recorded when a job is created, so `results export` JSON now always has `versionDisplay` and `supersedesCatalogId` keys (empty outside a document); `run status` shows the document version of a versioned catalog. `ParseCatalogResponse` gained `version_label`, the stored label, which the ingest attestation now records as its `version_label` by-product. `catalog import --json` gained `version_display` (`slug @ label (date · hash)`), `superseded_catalog_id` and `change_report` (`added`, `removed`, `changed`, `ids_unstable`), present only for an import under a document. `catalog list --json` and `catalog inspect --json` gained `document_slug`, `version_label`, `version_display`, `supersedes_catalog_id`, `link_method` and `current` for a catalog under a document; their `version` is still the OSCAL metadata version. `catalog list` adds a VERSION column only when a catalog is under a document. Unversioned output is unchanged.
- New `catalog retire <slug>@<label>...` and `RetireCatalogVersion` RPC (admin only) archive and delete old versions. The command requires `--yes` (scripts must pass it) because retiring is not reversible from the CLI; see "Retire old versions" under step 6. Run `crosscodexd migrate` to apply migration `002` (the daemon never migrates). It adds `catalog_documents` and the lineage columns, and also grants `purge_user` `SELECT` on `catalog_documents` and `UPDATE (supersedes_catalog_id)` on `catalogs`, so a database migrated to version 2 by an earlier build of this branch must be recreated.
- Retention purge of an expired catalog version now links the version that superseded it to its predecessor instead of failing on the lineage foreign key. An expired document's current version is kept: the scan counts it as held and does not archive or purge it.

Independent panels, sanity check and critique (#260):

- **Stages stop when the model stops answering.** New `analysis.engine.success_timeout` (default 10m, range (0, 24h], at most `pipeline.stage_timeout`; a config that sets it longer no longer loads). A stage that receives only failed task results, and no successful one, for that long now fails with error class `llm_unavailable` and abandons its queued tasks. If its last failure was an answer cut off at `max_tokens`, it fails as `llm_output_truncated` with that advice instead. Before, each failed result reset `analysis.engine.task_timeout`, so a stage whose every LLM call failed ran until `pipeline.stage_timeout` (24h). The window starts at the first failure after the stage's last success, and the stage fails only while its own failures keep arriving (each within `task_timeout` of the previous; a longer gap restarts the window). It must be at least `task_timeout`. A stage waiting behind another stage's queue, which receives no results of its own, is not failed by it. If your model server legitimately takes longer than 10m between successful answers while some calls fail, raise it. Resume a failed job with `crosscodex run retry --from-failure <job-id>`.
- **Aliases.** The `relationship`, `requires` and new `critique` panels now ask three different models through the gateway aliases `panel-1`, `panel-2` and `panel-3` (`deploy/config.yaml`), one sample each, instead of `default` three times. Every gateway config must define the three aliases, each pointing at a different model: add them to your LiteLLM or Praxis config (see [Panel aliases](deploy/README.md#panel-aliases)) or the panel stages fail on the first call. `default` stays the classification alias and `embed` the embedding alias. If you keep your own `analysis.*.models`, list each alias once: a config that names an alias twice in `analysis.relationship.models`, `analysis.requires.models`, `analysis.critique.models` or `analysis.artifact_adjudication.models` fails to load, naming the repeat.
- **Grouped dispatch.** A panel stage now sends its tasks grouped by model alias, in panel order, so a model server that loads models on demand swaps models a few times per stage instead of on nearly every task. Results are unchanged, with one exception: a panel model that fails every call now delivers its failures back to back, so after `analysis.engine.success_timeout` the stage stops with that model's error class (`llm_unavailable` or `llm_output_truncated`) and its advice, where before the other models' answers kept it running and it finished on two models' votes. Fix or remove the failing model (see [A stage stops: "it received only failed task results"](deploy/README.md#a-stage-stops-it-received-only-failed-task-results)), or raise `analysis.engine.success_timeout`. Intermittent failures of one model among its successes do not stop the stage.
- **Diversity check, fail closed.** Before a panel stage's calls, the daemon sends each alias one minimal request and compares the upstream models. A panel with fewer than two distinct aliases (for example `models: [default]` with `samples_per_model: 3`, the setup before this release), two aliases that answer as one model, or an alias whose model the gateway does not name, stop the stage with an error saying how to fix it. Set `analysis.relationship.allow_shared_panel_models`, `analysis.requires.allow_shared_panel_models` or `analysis.critique.allow_shared_panel_models` to `true` (each default `false`, global-only) to run a single-model panel anyway, as an offline Ollama stack must. With the allowance, a model that cannot be determined, including one whose catalog read failed (the fault is recorded), is recorded and the stage runs; only a failed probe call (gateway down, key refused on the call) still fails it. Without it, a failed catalog read fails the stage. The check reads LiteLLM's `GET /v1/model/info`, so the key `crosscodexd` uses (`llm.api_key_ref`) must be allowed to read it, and a gateway without that endpoint fails closed unless the allowance is set. `crosscodexd` refuses to start with an LLM probe client that cannot read a model catalog. `crosscodex run status` prints, per panel stage under `Panels:`, each alias with the model it reported and where that came from (`reply`, `catalog` or `unknown`, with the fault if the catalog read failed) and whether the allowance was on; `--json` carries the same as `panels` (including each alias's `deployment_id`). A stage from before the check shows "panel independence not recorded". The stage's signed attestation link records the same as `panel.models`, `panel.allow_shared` and `panel.independence`.
- **Sanity check.** The `relationship` (1.3.0) and `requires` (1.4.0) prompts now ask each voter for a final `SANITY_CHECK: PASS|FAIL` line (could it name the shared obligation, and would an auditor accept the target's evidence toward the source). `FAIL` turns the vote into `NO_RELATIONSHIP` / `NO` and keeps its justification. A reply without the line, or with another value, is an invalid vote, which counts toward `max_error_rate`. The `requires` prompt now answers with `YES` or `NO` only; the `NO_DEPENDENCY` and `BENEFITS_FROM` labels are gone. Prompt overrides are not version-checked: add the line to yours by hand or every vote of that analyzer is invalid.
- **Critique stage, on by default.** `analysis.critique.enabled` defaults to `true` (panel `panel-1..3`, one sample each). Whenever `relationship` or `requires` is enabled, a third panel re-reads every verdict they accepted (every relationship that is not `NO_RELATIONSHIP`, every REQUIRES `YES`) and answers `UPHOLD` or `REJECT`; a verdict is rejected when the REJECT share of valid votes reaches `analysis.critique.consensus_threshold` (default 0.66). By default (`analysis.critique.veto: false`) a rejected verdict stays a mapping and keeps its graph edge, flagged with critique outcome `rejected` and a `critique rejected: ...` note, so you can see the critics' doubt without losing the pair; `crosscodex results export --stage analyzer --exclude-rejected` (or `summary` with the same flags) gives the strict view of the same job. Rejected pairs also reach synthesis, so the default (`synthesized`) export and the graph's synthesized edges include them; that listing carries no critique to filter on, so for a strict synthesized result run the job with veto on. Set `analysis.critique.veto: true` (`CROSSCODEX_ANALYSIS_CRITIQUE_VETO=true`, global-only) to drop rejected verdicts instead: a vetoed verdict is not a mapping and writes no graph edge, so expect fewer mappings and REQUIRES edges. In the #260 crosswalk eval, veto raised labelled precision to 80–87% but dropped about 40% of the correct rows, which is why it is off by default. The setting is applied when the critique stage decides, so each job's stored critique records which behaviour it ran with, and changing it does not invalidate cached critique calls. It adds up to panel size (`models` × `samples_per_model`, default 3) calls per accepted pair, on top of the relationship and requires calls (see Cost under [8. Start an analysis](#8-start-an-analysis)). To turn it off, set `analysis.critique.enabled: false` (`CROSSCODEX_ANALYSIS_CRITIQUE_ENABLED=false`); verdicts are then final as before. With both pair analyzers disabled it is not registered and the daemon logs why. Vetoed pairs stay readable: `crosscodex results export --stage analyzer --include-vetoed`.
- **Reasoning models.** The shipped `deploy/config.yaml` sets `max_tokens: 2048` for `analysis.relationship`, `analysis.requires` and `analysis.critique` (compiled default still 300), because a reasoning model spends completion tokens before answering; the openai-compat smoke gateway template now also sends `reasoning_effort: "low"` beside `enable_thinking: false` on every chat alias, which caps gpt-oss reasoning (other models ignore it). Keep your own config's `max_tokens` at least that high for a reasoning panel model, or its stage fails as `llm_output_truncated`. The budget is a ceiling: a model that answers directly stops sooner and is billed only for the tokens it used. A changed `max_tokens` changes the requests, so that analyzer's cached results miss once and are recomputed.
- **Cache.** The prompt versions changed (`relationship` 1.3.0, `requires` 1.4.0, new `critique` 1.0.0) and the panel models changed, so cached relationship and requires results miss once and are recomputed. Expect that one-time cost on the first run after the upgrade.
- **Database.** `job_stages` gains the column `panel_record`, folded into the baseline migration `001_initial_schema`. A database created by an earlier build must be recreated (`task deploy:down VOLUMES=1`, start the stack, `crosscodexd migrate`); `crosscodexd` refuses to start on the old schema and names the missing column.
- **Jobs in flight.** If `relationship` or `requires` is enabled, a job created before the upgrade fails on resume, because its stored analyzer set lacks `critique` (see [Analyzer `enabled` settings](#analyzer-enabled-settings)). Start a new job.
- **Roll out the graph role first.** Upgrade the graph role before or together with the pipeline role. An old graph subscriber ignores the `graph_deferred` event metadata, writes edges the critique later vetoes (the graph cannot delete them) and skips the `critique` event. A job that was in flight across the upgrade may keep such edges; re-run it.
- **Crash window.** The critique's results are saved before its event is published. A crash between the two leaves that job's `relationship` and `requires` edges unwritten in the graph, and a resume does not publish the event again. Re-run the job.
- **Output.** `SEMANTIC_MATCH` and `REQUIRES` edges kept by a critique carry `critique_upheld`, `critique_rejected`, `critique_outcome` (`upheld`, `split`, `rejected` or `failed`) and, for a split, rejected or failed critique, `critique_note`; edges of jobs without a critique carry none. `results export` gains `--include-vetoed`, `--exclude-rejected` and a `critique` key with the outcome, and analyzer-stage CSV gains a last `critique` column; crosswalk `review` shows the critique notes. These are additive.
- **Telemetry.** New metrics `crosscodex.pipeline.panel_checks`, `crosscodex.critique.verdicts`, `crosscodex.critique.duration_ms` and `llmclient.model_catalog.total` (see [docs/dev/telemetry.md](docs/dev/telemetry.md)).
- Compare runs from the same build only.

OSCAL control tree (#260):

- Upgrading requires recreating the database: OSCAL groups moved out of `controls` into a new tenant-scoped table, `catalog_containers`, which the pre-release baseline migration now creates. A database from an earlier build is refused at startup with a message saying so. Recreate it (compose deployment: `task deploy:down VOLUMES=1`), run `crosscodexd migrate` on the fresh database, start `crosscodexd`, and re-import your catalogs.
- Groups are no longer controls. Each group is stored as a container with a position key (`g1`, `g1.1`, ...); its source `id`, if any, is only its displayed identifier, and a group without an `id` (for example the groups of the ISM Essential Eight profile) is shown by its title alone. `controls` holds only controls (`node-kind` `control` or `statement-part`), so `control_count`, search and `GetControl` no longer include groups, and control counts do not rise by the number of groups.
- The graph holds each group as a `Container` node (`<catalog_id>/containers/<key>`, `node_kind` `container`, plus `prose` holding the group's own overview text when it has any; never analysed as a requirement), linked `PARENT_OF` from its parent group and to its top-level controls. Enhancements are linked to their base control as before.
- Every control row states its role in `parts` (visible in `GetControl`) as `node-kind`: `control` or `statement-part`. The graph `Control` node carries the same value as `node_kind`.
- Analysis input is composed once per control. Each control carries a context (its titled ancestors, root first), a usable title (one that only repeats the control's identifier or published label, or respells it with a digit, such as "MANAGE 4.2" for `MANAGE_4.2`, is dropped) and its statement; a statement part's statement is prefixed with the prose of the statement parts and control above it, up to its owning control. A control with a usable title but no requirement text is analysed on its title. Controls the catalog marks as sections or withdrawn, and controls with neither text nor a usable title, are not analysed. This changes the `artifacts`, `classify`, `requires` and `relationship` prompts and the embedding text (now `[<context>] <title>: <statement>`), so cached results for relationship, requires, artifacts, classify and embeddings miss once and are recomputed. No action needed beyond re-importing your catalogs; expect that one-time cost on the first run.
- Crosswalk context breadcrumbs now show the full path, for example `Access Control (AC) > Account Management (AC-2) > Privileged User Accounts (AC-2(7))`: each ancestor is its title and, in brackets, the label the catalog publishes, else its identifier; a group without either shows only its title.
- `CONTRIBUTES_TO` (the relationship type formerly read `contributes to`) now reads `is advanced by`: the target advances the source. Exports and the crosswalk show the new wording; nothing stored changes.
- Crosswalk exports have new columns `source_label`, `source_parent_label`, `target_label`, `target_parent_label` and `hub_label`, placed as listed under "Crosswalk"; scripts that read export columns by position must be updated, and ones that read by header name keep working. `CrosswalkSide` gained `label` and `parent_label`, and `CrosswalkRow` gained `hub_label`; these are additive.
- The text of a control is now the first of the OSCAL parts named in the new `catalog.requirement_part_names` (default `[statement]`, global-only) that the control has. A control that is not withdrawn and has no such part and no usable title is reported: `catalog import` lists up to ten of them with the part names they do have, and `--json` carries all of them. Re-import after adding the right part name to the config.
- `catalog import --json` changed shape. It was `{"catalog_id", "document_id"}`; it is now an object with those two fields plus `textless_controls`, an array (empty when none) of `{"identifier", "part_names"}`. Readers of the two old fields keep working; code that compared the whole object does not. The same list is `textless_controls` on `ParseCatalogResponse` and `SubmitDocumentResponse`.
- Import now rejects a control without an `id` and two items (controls or statement parts) with the same ID, naming them. Fix the source catalog, or remove the item.
- Prompt variables were renamed: `source_ancestor` is now `source_context`, `target_ancestor` is `target_context`, and the single-control `ancestor_title` is `context`; `source_title`, `target_title` and `title` are new. `source_context` and `target_context` hold the full ancestor path, not just the parent's title. `classify` now also receives `context` and `title`. A user or project prompt override that still uses an old name fails to render with a missing-placeholder error: rename the variables by hand (prompt overrides are not version-checked). Shipped prompt versions are now relationship 1.2.0, requires 1.3.0, artifacts 1.2.0 and classify 1.2.0; the `requires` prompt no longer renders a "Control Domain" suffix.
- Import rejects a catalog with a group whose title is empty or blank (`InvalidArgument`, naming the group's `id` and position). OSCAL requires every group to have a title; give the group one, or move its controls to the catalog's top level.
- `pkg/oscal`'s `Assembler` no longer wraps controls that have no group in a placeholder group (`id` `default`, empty title); they become top-level controls. A catalog assembled by an earlier build may contain that placeholder and is now rejected on import: re-assemble it, or remove the group as above.
- Compare runs from the same build only.

OSCAL parameter text (#260):

- Label-only OSCAL parameters now render as `[Assignment: <label>]`, and a parameter's set value is used over its label (a resolved profile's chosen value was previously hidden by the label).
- Re-importing such a catalog changes its control statements, so cached LLM results for those controls miss once, their embeddings are recomputed, and jobs re-run against the new text.
- Compare runs from the same build only.

Crosswalk export (#260):

- `GetCrosswalk` (gateway) and `crosscodex crosswalk` are new.
- The graph now holds a second `SEMANTIC_MATCH` edge per judged pair per job, with the new property `stage: synthesized` (plus `viability` and `is_viable`); analyzer edges get `stage: analyzer`. Anything that counts or traverses `SEMANTIC_MATCH` (admin `QueryGraph`, `Traverse`, ad-hoc Cypher) now sees twice as many edges unless it filters on `stage`. Edges written before the upgrade have no `stage`; crosswalks read them as analyzer edges.
- Jobs completed before the upgrade have no synthesized edges, so the default `crosscodex crosswalk` (stage `synthesized`) is empty for them. Re-run those jobs (`crosscodex run start ...`; cached results make it cheap) or read the analyzer's verdicts with `--stage analyzer`.

Bounded multi-generator candidates (#259):

- `aggregate_score` in `requires` provenance is now a fused ranking value, the sum of weight / (60 + rank) over the generators that found the target, not a weighted average of scores. Compare it only within one source's candidates.
- Each candidate's `requires` provenance lists every generator that contributed, with its `rank` in that generator's list for the source (tied scores share the mean position).
- A relationship pair's `similarity_score` is always the embedding cosine of the pair, whichever generators found it.
- Candidates are capped per source by the new `analysis.candidates.max_per_source` (default 10, at least 1), so keyword and level generators no longer send every match to the pair analyzers; keyword and level give every match the same score, so when they are the only generators that find a source's targets, the targets kept under `max_per_source` are the first by target ID, not the most relevant. A config where an enabled semantic or lexical `top_k` (default 10 when unset) exceeds it fails to load; raise `max_per_source` or lower `top_k`.
- New `lexical` generator (BM25 over control text), disabled by default.

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
- A key written through an alias (`*k: debug`) that names the same setting as another key in its mapping is refused, with both lines (#295). Before, the later key silently won. Remove one of the two keys.
- `crosscodex config set <key> <value>` and the new `crosscodex config unset <key>` edit only the user config, and write it only if the configuration loads with the change, every other layer and `--profile` included (only that profile; others are not checked). A misspelled key, an invalid value, a key that names a section or list, a path through an alias (`<path> is an alias to another section`), or a change to a section or value whose anchor an alias in the file refers to (the error names the aliases), is refused with the reason, and the file is left byte-identical. When a change depends on another setting (such as `tls.mode mutual` on `tls.cert`, `tls.key` and `tls.ca`), set that one first. The file is replaced atomically, keeping its permissions and owner (a new one is created `0600`; a config owned by another user is refused unless the command can give the new file that owner), and a symlinked user config is written through to its target; a dangling symlink is refused.
- When the configuration does not load, every command fails with the error, except the `crosscodex config` commands: they print it as a warning and still run. If the user config is the cause, `config unset` removes an unknown key and `config set` fixes a value, one problem per change: a change is written if it removes at least one problem and adds none (the remaining ones are printed as a warning), and refused if it adds a problem or replaces a bad value with another bad one. A rule spanning settings counts as one problem under the setting that imposes it, so `tls.mode mutual` stays a problem until `tls.cert`, `tls.key` and `tls.ca` are all set: set those first, or `tls.mode` back to `off`. If some problem has no setting path (a document whose root is not a mapping, such as a YAML list; a file that does not parse is refused outright, fix its syntax by hand), a change is written only if it makes the configuration load. If a problem's value comes from another layer (system config, drop-in, profile, environment variable), set and unset refuse and name it: fix that file first. A layer that only loads together with the user config (a drop-in sets `llm.gateway_mode: true`, the user config sets `llm.gateway_url`) does not count as broken. A map key a dotted path cannot hold (one containing `.`, `[`, `]` or `"`) is written quoted, both in errors and in `config set`, `get` and `unset` keys, for example `crosscodex config unset 'llm.tenant_overrides["a.b"]'`. `config show` and `config get` have no resolved configuration to print, so they report the error once instead.

Tenant administration (#31):

- The gateway refuses every non-health request, with `PermissionDenied`, from a tenant whose `tenants` row is missing, suspended or deleted. The initial schema creates the `tenant_admin` role that `crosscodexd admin tenant` connects as, so tenants can only be provisioned after `crosscodexd migrate` runs. Once crosscodexd has migrated, set the role's password and `CROSSCODEX_DATABASE_TENANT_ADMIN_DSN`, then run `crosscodexd admin tenant create` for each authenticated tenant before sending it traffic; requests from that tenant are refused until then. Roles `all` and `gateway` refuse to start until `crosscodexd migrate` has provisioned `tenants.default_tenant`. See [Tenant administration](deploy/README.md#tenant-administration).
- The gateway refuses, with `PermissionDenied`, a request whose `tenant_context` names a tenant other than the one its client certificate authenticates. Before, the gateway replaced the named tenant with the certificate's and answered for that one, so `crosscodex catalog list --tenant acme` with a certificate for `default` listed `default`'s catalogs, and `run start --tenant acme` started a job for `default`. Omit `--tenant` (it defaults to the certificate's tenant), or connect with a certificate issued for the tenant you name.

Analysis stage limits (#237):

- Timed-out LLM calls are no longer re-sent by default. A call that hits the client's deadline, or that the gateway or provider answers with HTTP 408 or 504, fails its task as `timeout` with one upstream attempt, because the provider may already have billed it. Before, such a task was re-sent up to `analysis.engine.max_retries` times (and in direct mode the client itself also retried a deadline or a 504, up to `llm.max_retries` times). Set `analysis.engine.retry_on_timeout: true` (tenant-overridable through `analysis.engine.tenant_overrides`) to re-send them. A malformed task payload (`task_invalid`), a tenant mismatch and an unstored audit record are never re-sent. `analysis.tasks.retried` gains a `cause` attribute (`timeout`, `rate_limit`, `other`). The per-task bound on upstream calls is in [Task retries](#task-retries).
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

- `deploy/config.yaml` now enables `analysis.relationship` and `analysis.requires`, with the `semantic` candidate generator. Before, both were off, so a job on the compose stack completed with zero mappings. A job now makes pair calls as well: about source controls × 10 × 6 (relationship 3 + requires 3), plus critique calls, so it takes longer and costs more (see [deploy/README.md](deploy/README.md#what-an-analysis-runs-and-what-it-costs)). Set `enabled: false` there to go back. Their panels use the aliases `panel-1..3`, which your gateway config must define (see "Independent panels" above).
- The compiled defaults are unchanged: a `crosscodexd` with no config file still runs no pair analyzers.

Mapping confidence:

- A relationship mapping's confidence is now the panel's agreement times the mean `analysis.relationship.confidence_weights` weight of the winning votes' reported `CONFIDENCE` (see "Confidence" under [Read a job's mappings](#9-read-a-jobs-mappings)). Before, it was the agreement alone, so a one-sample panel gave every mapping 1.000 and `--min-confidence` and `synthesis.confidence_threshold` filtered nothing. Jobs run after the upgrade report lower confidences than earlier jobs on the same catalogs, and with the default weights and threshold a one-sample panel's LOW mappings are no longer viable. Compare runs from the same build only. Set all three weights to 1 to get the old values back.
- The agreement alone is a new `agreement_fraction` property on each `SEMANTIC_MATCH` graph edge and in the relationship analyzer's stored results.
- Cached LLM results are still reused: the prompts and requests are unchanged, and confidence is computed from the cached responses when a job aggregates them. Changing `samples_per_model` does change the requests (a panel of more than one sample uses `sampling_temperature` instead of 0), so the compose stack's switch to the `panel-1..3` aliases (one sample each) sends its relationship and requires requests to the model again once.
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

**Prerequisites**: Go >= 1.26, Task (taskfile.dev), container engine (podman or docker)

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

Import stores and parses the catalog and prints its catalog ID. It does not start analysis. If some controls that are not withdrawn have no requirement text (none of the parts in `catalog.requirement_part_names`) and no usable title, import lists them: they will not be analysed.

To track editions of one document, import each under the same slug. The newest import becomes the document's current version, and the output counts the controls added, removed and changed against the version it superseded (`--json` lists their identifiers):

```sh
crosscodex catalog import --document nist-800-53 --version-label "Rev 5" catalogs/NIST_SP-800-53_rev5_catalog.json
```

A slug is 1-63 characters of `a-z`, `0-9` and `-`. Without `--version-label` the label is the OSCAL metadata version, else the import date and a short content hash. Re-importing content the document already holds keeps its version and label; giving it a different `--version-label` is refused (relabelling is not supported). `catalog list` then shows `slug @ label (date · hash)` with the current version marked; catalogs imported without `--document` list as before.

Retire old versions you no longer need (admin role; the server must run role `all` or `gateway` with `database.purge_dsn` and `retention.archive` set):

```sh
crosscodex catalog retire --yes "nist-800-53@Rev 3" "nist-800-53@Rev 4"
```

Without `--yes` the command refuses and lists the `<slug>@<label>` versions it would retire; run `crosscodex catalog list` to review them first. Each version is archived, then deleted with its controls, embeddings and cached results. **Retiring cannot be undone from the CLI:** there is no restore command, and the archive copy is the only one left. The version that superseded it is linked to its predecessor. Versions named in one command are retired together or not at all. Retire refuses, naming the blocker, a document's current version, a version a legal hold covers, and a version used by a job that is still pending or running, is under a legal hold or has attestations. Graph nodes are left in place. See [docs/dev/retention.md](docs/dev/retention.md#retiring-a-catalog-version) and, for the lineage model and limits, [docs/dev/catalog-versions.md](docs/dev/catalog-versions.md).

#### Import a document (PDF, HTML, DOCX)

With the ingestion service configured (`ingestion.address`, see [Document ingestion](#document-ingestion)), `catalog import` also takes a PDF, HTML, DOCX or text document; no flag is needed. The CLI sends a file that is OSCAL JSON as OSCAL and lets the server detect the format of any other file (or of every file, with `--raw`). The document is converted by [crosscodex-ingestion](https://github.com/complytime-labs/crosscodex-ingestion) (Docling), then split into controls by the heading, table and numbering rules:

```sh
crosscodex catalog import --document sp800-171 --version-label "Rev 3" nist-sp800-171r3.pdf
```

Conversion can take minutes for a long PDF. The import fails and no catalog is stored when:

- the document is larger than `ingestion.max_document_bytes` (50 MiB by default; `ResourceExhausted`, refused before upload to the service);
- conversion takes longer than `ingestion.convert_timeout` (10 minutes by default);
- no heading, table or numbering rule in the converted text finds the controls (`FailedPrecondition` naming `catalog.structuring.section_pattern`: set it to a regular expression whose group 1 is the control identifier and group 2 its text, then import again).

A failed import can still leave objects in the tenant's storage; see the orphaned-output limitation under [Document ingestion](#document-ingestion).

Without ingestion configured, a document that is not OSCAL JSON is rejected as before.

### 7. List and inspect catalogs

```sh
# List all imported catalogs
crosscodex catalog list

# Inspect a specific catalog (use the ID from the list output)
crosscodex catalog inspect <catalog-id>

# List a catalog's controls with the title and statement analysis gives each analyzer
crosscodex catalog controls <catalog-id>
```

`catalog controls` shows each control as the analyzers see it: a statement part carries its parents' lead-ins, a title that only repeats the control's label is dropped, and a title-only control's title becomes its statement. Use it to find the control IDs that `prompt render` takes. The whole catalog arrives in one response, so a large catalog is one large reply. The text is the catalog's own and untrusted: text output strips control and invisible format characters, `--json` returns it unchanged and JSON-escaped; render neither as markup (HTML, Markdown). `catalog list --json` and `catalog inspect --json` carry `content_hash`, the SHA-256 of the bytes the catalog was imported from (`""` when none was recorded).

### 8. Start an analysis

Analysis runs in the `crosscodexd` daemon. To try it on this machine without an API key, bring up a persistent, offline stack (a small local model, its own ports, volumes and certificates; see [Try it locally](deploy/README.md#try-it-locally)):

```sh
cp deploy/.env.example deploy/.env   # then set the three passwords: openssl rand -hex 24
task deploy:local
```

The task builds the CLI and prints how to call it with the stack's client certificate. Check that it answers:

```sh
.local/crosscodex/bin/crosscodex --endpoint https://localhost:50251 \
  --tls-ca .local/crosscodex/pki/ca.pem \
  --tls-cert .local/crosscodex/pki/client.pem \
  --tls-key .local/crosscodex/pki/client-key.pem \
  catalog list
```

The stack has its own database, so import the catalogs into it with `catalog import` using the same flags. The commands below take those flags too. `task deploy:local:down` stops the stack and keeps its data; `task deploy:local:down VOLUMES=1` also drops the data, key volumes and PKI.

An analysis maps the controls of a source catalog onto a target catalog (a crosswalk, for example CSF 2.0 onto SP 800-53):

```sh
crosscodex run start --source <catalog-id> --target <catalog-id>
crosscodex run status <job-id>
```

- **Direction.** Pairs run source to target: each source control is compared with its most similar target controls. NIST IR 8477 relationship types are directional ("A is a subset of B" is not "B is a subset of A"). `--bidirectional` also maps the target's controls onto the source's. For target to source only, swap `--source` and `--target`.
- **`--target` is required.** To analyse within one catalog (for example to find overlaps and REQUIRES dependencies in a catalog that carries no links of its own), pass its ID to both: `crosscodex run start --source <id> --target <id>`. Within one catalog, controls are already paired in both directions, so `--bidirectional` is refused there.
- **What runs.** The job classifies, embeds and extracts artifacts for the controls of both catalogs, then runs the pair analyzers (`requires`, `relationship`) over the candidate pairs, then the critique, which re-reads the verdicts they accepted and flags the ones it judges wrong as `rejected` (or drops them, with `analysis.critique.veto: true`; see "Independent panels" under Upgrading). Each pair prompt names both requirements' frameworks, taken from the catalog names. A crosswalk whose target has no non-section, non-withdrawn control with a statement fails before any analyzer runs.
- **Cost.** Groups (stored as containers, not controls), sections and withdrawn controls (OSCAL `status` prop `withdrawn`, stored as the control prop `status`; their `incorporated-into` / `moved-to` targets are kept too) make no LLM calls, and `embedding` also skips controls without a statement. For every other control of both catalogs (a job within one catalog counts it once), each enabled per-control analyzer makes: `classify` 1 call, `embedding` 1 per model in `analysis.embedding.models`, `artifacts` `models` × `samples_per_model`. Pair calls ≤ source controls × `max_per_source` (`analysis.candidates.max_per_source`, default 10) × panel size (`models` × `samples_per_model`), summed over the enabled pair analyzers (`analysis.requires`, `analysis.relationship`). The critique (`analysis.critique`, on by default) adds panel size (`models` × `samples_per_model`, default 3) calls per critiqued verdict: a relationship other than `NO_RELATIONSHIP`, or a REQUIRES `YES`. A pair proposed as both kinds is critiqued twice, so the ceiling per pair is two panels. On a catalog where most pairs are rejected the critique is a small addition; where most are accepted it adds up to as many calls as the relationship panel. Each panel stage also makes one probe call per alias before its calls, to check that the aliases are different models, and at most one read of the gateway's `/v1/model/info`, only when an alias's reply echoes the alias itself and gives a deployment ID. `--bidirectional` adds the reverse direction, whose sources are the target's controls, so pair calls become ≤ (source + target controls) × `max_per_source` × panel size, summed over the same analyzers: mapping CSF (about 100 controls) onto SP 800-53 (about 1,000) costs about 11 times the one-way pair calls, not twice. `crosscodex catalog list` and `catalog inspect` show each catalog's control count (sections and withdrawn controls included, so an upper bound; SP 800-53 rev 5 has about 200 withdrawn) before you start; `run status` shows both sides' while the job runs.
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
    - The gateway stops answering, or the model's answers fail, mid-stage: once the stage has received only failed results, and no successful one, for `analysis.engine.success_timeout` (default 10m) while its failures keep arriving, the stage stops (`llm_unavailable`, retryable). See [Troubleshooting](deploy/README.md#a-stage-stops-it-received-only-failed-task-results).
  - **Retention.** Cached results are purged with the job that produced them, and with either catalog whose text they carried.
- **Seeing what ran.** `crosscodex run status <job-id>` shows the source and target catalogs (name, version, ID and control count) and the direction. `crosscodex results export` carries the catalogs (ID, name, version) and direction: JSON under `analysis`, CSV in the `job_*` columns, which describe the job, not the row (in a bidirectional job a row's source control can come from the job's target catalog). Names and versions are the ones recorded when the job was created; for a job created before #210 they are the catalog's current ones, which `run status` marks "(current name; not recorded when the job was created)" and export marks with `snapshot: false` / `job_*_catalog_snapshot` `false`. A version shows as "not recorded" when import stored none. For a catalog imported under `--document`, `run status` adds `document version slug @ label (date · hash)` to the catalog's line and `--json` adds `version_display` and `supersedes_catalog_id`; `results export` and crosswalk export JSON carry `versionDisplay` and `supersedesCatalogId` in each catalog summary (empty outside a document). The CSV columns are unchanged. A CSV cell taken from a catalog that would start with `=`, `+`, `-` or `@` gets a leading `'` so a spreadsheet does not run it as a formula. JSON key names differ between the two commands: `run status --json` uses snake_case (`source_catalog`), while `results export` renders `analysis` and each mapping as proto JSON, in camelCase (`sourceCatalog`, `mappingId`), as mappings were before #210; only its top-level `job_id` is snake_case.

`run start` prints the job ID. Analysis runs in the `crosscodexd` daemon; embedded mode refuses it (see [Deployment Modes](#deployment-modes)).

**Seeing progress.** While a job runs, `run status` shows each stage that dispatched tasks as one row:

```text
stage  status  done/total  ok N  cached N  failed N  retried N  last success 42s ago
```

- `total` is the tasks the stage dispatched. `done` is `ok` plus `failed`: the tasks that finished.
- `ok` includes the `cached` tasks, which the result cache answered instead of the model.
- `retried` counts retry attempts, not tasks: a task retried twice adds 2.
- `last success` is the time since a worker last completed a task, or `never`. A cache hit does not move it, so a stage served from the cache can show `never` while `done` rises.
- A resumed stage adds to its earlier counts, so `done` can exceed `total`.
- A stage with no recorded progress (not started, or run before this was recorded) has no row, and no `progress` object in `--json`. `--json` lists `tasks_total`, `tasks_done`, `tasks_succeeded`, `tasks_failed`, `tasks_retried`, `tasks_cache_hits` and `last_success_at` under `stages[].progress`.
- Counts reach the database every `analysis.engine.progress_flush_interval` (default 10s), so they lag the stage by up to that long, and again when the stage ends. If the database refuses a write the figures can stay behind until the next one; the daemon logs it.

How the counts are written and what they cannot show: [docs/dev/stage-progress.md](docs/dev/stage-progress.md).

**Seeing a prompt.** `crosscodex prompt render` prints the messages the `relationship`, `requires` or `critique` analyzer sends its models for one control pair, built by the analyzer's own prompt code from the stored controls. It calls no model:

```sh
crosscodex prompt render --analyzer relationship --source r3/SR-03.01.01 --target ssdf/PO.1.1 \
  --source-catalog r3 --target-catalog ssdf
crosscodex prompt render --analyzer requires --job <job-id> --source r3/SR-03.01.01 --target ssdf/PO.1.1
crosscodex prompt render --analyzer critique --job <job-id> --source r3/SR-03.01.01 --target ssdf/PO.1.1
```

- **The messages are untrusted text.** They carry catalog text. The text output prints them verbatim between `--- <role> ---` fences and `--json` JSON-escapes them. Never render either as markup.
- **It shows the current configuration, not the job's.** The prompt templates and analysis settings (statement length bounds) are those of the daemon, or of embedded mode, running now. If they changed since the job ran, the prompt differs from the one the job sent.
- **Without `--job`** the controls' types and levels are absent, and each framework is the catalog's current name. With `--job` the job's catalogs, the catalog names it recorded and its stored classify result are used, and `--source-catalog` and `--target-catalog` default to the job's.
- **`critique` reviews one verdict.** Give exactly one of `--job` and `--verdict`. `--job` uses the verdict that job accepted for the pair, and is refused when the job accepted none, or accepted both a relationship and a REQUIRES for it (its critique stage reviewed each with its own prompt, and a render shows one): then use `--verdict` instead of `--job`, naming a relationship type such as `EQUIVALENT` or `REQUIRES`, with the job's justifications as repeatable `--justification`. `--verdict` and `--justification` apply to `critique` only. `--justification` needs `--verdict` and cannot be combined with `--job`.
- **Access.** Rendering from catalogs alone is open to any user of the tenant, as reading the catalogs is. With `--job` it shows the job's classifications and verdicts, so only the job's owner or an admin may use it; anyone else gets `PermissionDenied`. An unknown job, another tenant's job and an unknown control all answer `NotFound`.
- **`--tenant`** names the tenant to act on. It must be the tenant your client certificate authenticates; by default that tenant is used.
- **Limits.** IDs are limited to 256 bytes and `--justification` to 64 entries.
- It works in embedded mode, which renders from the local database. How it works: [docs/dev/prompt-render.md](docs/dev/prompt-render.md).

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

# Export a completed job as an OSCAL mapping collection (see docs/oscal-mapping-export.md)
crosscodex results export <job-id> --format oscal-mapping --output mapping.json
```

`--format oscal-mapping` writes an OSCAL 1.2.3 mapping collection, marked `draft`. [What it contains, what it leaves out and when it refuses](docs/oscal-mapping-export.md).

| Flag | Applies to | Meaning |
|------|------------|---------|
| `--stage synthesized\|analyzer` | both | `synthesized` (default) is the output after synthesis's confidence threshold and per-control cap, with a viability score per mapping. `analyzer` is the raw relationship analyzer consensus, with no viability: `export` omits `viabilityScore` and `isViable` from JSON and leaves the `viability_score` and `is_viable` CSV cells empty. |
| `--min-confidence <0-1>` | both | Only mappings at or above this confidence. |
| `--type <type>` | both | Only this relationship type; repeat for several. Case-insensitive. `NO_RELATIONSHIP` is excluded unless named here. |
| `--control <id>` | both | Only mappings whose source or target is this control. |
| `--include-non-viable` | both | Also return pairs synthesis filtered out (`synthesized` stage only). By default only viable mappings are returned. |
| `--include-vetoed` | `export` | Also list pairs the critique stage vetoed (`analyzer` stage only; synthesis never receives them). By default a vetoed pair is not a mapping, and `summary` refuses the flag because it counts only mappings. Their `critique` CSV cell reads `vetoed: 0 upheld, 3 rejected, 0 failed; <reasons>`. Every analyzer-stage CSV export has that `critique` column (followed by `cleaning_state`), whose cell starts with the outcome (`rejected: ...`, `split: ...`) and is empty for an unreviewed mapping; JSON carries every reviewed mapping's `critique` with its outcome, votes and reasons. |
| `--exclude-rejected` | `export`, `summary` | Leave out pairs the critique rejected without veto (outcome `rejected`, from a job run with `analysis.critique.veto: false`), for the strict view of that job (`analyzer` stage only; synthesized mappings are listed without their critique). Combines with `--include-vetoed`, which selects a different outcome. |
| `--format json\|csv` | `export` | Output format (default `json`). |
| `--output <file>` | `export` | Write to a file instead of stdout. |
| `--json` | `summary` | Structured output. |

To see which pairs candidate generation proposed to the relationship analyzer, whether or not they ended up mapped, export the job's candidates:

```sh
crosscodex results candidates <job-id> --output candidates.json
crosscodex results candidates <job-id> --format csv --control <control-id>
```

`--format json|csv` (default `json`), `--output <file>` and `--control <id>` (source or target) work as in `export`. JSON has `job_id`, `analysis` and `candidates` (proto JSON, camelCase: `sourceControlId`, `targetControlId`, `similarityScore`, `contributions`). CSV has the columns `source_control_id`, `target_control_id`, `similarity_score` and `generators`, where `generators` joins the proposing generators with `;`. `similarityScore` is the embedding cosine similarity on a 0 to 100 scale, and 0 when no embedding exists for the pair. `contributions` (one per proposing generator, with its score and fusion weight) is empty unless the candidate strategy is rank fusion. `run status --json` also lists each stage's `name`, `status`, `started_at` and `completed_at` (RFC 3339; empty until set) under `stages`.

A job that is still pending or running returns an error saying mappings are available once it completes. A failed or cancelled job never produces mappings; start a new job. An unknown job ID returns not found. `mapping_id` equals the ID of the mapping's `SEMANTIC_MATCH` graph edge. `REQUIRES` is not a mapping and is not returned.

**Cleaning state.** A mapping that [edge cleaning](#edge-cleaning-crosscodex-admin-cleaning) or a reviewer's [determination](#mapping-feedback-crosscodex-results) changed is marked, and the stored results are unchanged. `export` JSON adds a `cleaning` object (`state` `retired`, `restored` or `stale`; `source` `finding` or `determination`; `refId`, the finding or determination ID; `changedAt`; `graphApplied`) only on such mappings. CSV ends with a `cleaning_state` column, empty for a mapping nothing changed. `crosscodex results debug <job-id> <control-id>` prints a `Cleaning: retired: finding 1a2b3c4d5e6f` line under such a mapping, with the ID cut to 12 characters; it shows the synthesized stage only. The latest change wins. A restored mapping lives under new edge IDs, but `mapping_id` is a stable key for the verdict and is unchanged.

**Confidence.** A mapping's confidence (0 to 1) is the relationship panel's agreement times how sure the agreeing votes said they were: (votes for the winning type / valid votes) × the mean of `analysis.relationship.confidence_weights` over the winning votes' reported `CONFIDENCE` (HIGH 1.0, MEDIUM 0.67, LOW 0.33 by default; a vote that reports none counts as LOW). Three samples that agree, all HIGH, give 1.0; two of three, HIGH and MEDIUM, give 0.557; a one-sample panel gives exactly its vote's weight. The winning type is still chosen by vote count alone. `--min-confidence` and `synthesis.confidence_threshold` (default 0.5) filter on this value, so with the default weights a one-sample panel's LOW mappings fall below the threshold. The agreement alone is kept as the `agreement_fraction` property of each `SEMANTIC_MATCH` graph edge.

### 10. Sharing crosswalks

`crosscodex crosswalk` exports the mappings between catalogs as one table a reviewer can read in a spreadsheet: each row names both controls (framework, identifier, parent, title, statement) and the relationship, so nobody has to decode IDs. The mode follows the arguments:

```sh
# Two catalogs: every pair judged between them, from any completed job in either direction, merged
crosscodex crosswalk <catalog-a> <catalog-b> --output crosswalk.csv

# One job: that job's verdicts
crosscodex crosswalk --job <job-id> --format json

# One catalog against every catalog it has completed jobs with (rows name the other framework)
crosscodex crosswalk <catalog-a> --all --output a-everywhere.csv

# Transitive: A to C derived through the hub catalog's controls (direction "derived"), plus the direct A to C rows ("direct")
crosscodex crosswalk <catalog-a> <catalog-c> --via <hub-catalog> --min-confidence 0.7
```

`--stage synthesized|analyzer` (default `synthesized`), `--min-confidence <0-1>`, `--type <type>` (repeatable) and `--include-non-viable` filter as in `results export`: the `synthesized` stage serves only the mappings synthesis judged viable unless `--include-non-viable` adds the pairs it filtered out (a flag the `analyzer` stage refuses, since analyzer mappings have no viability). `--type no_relationship` is refused: the relationship analyzer omits "no relationship" verdicts, so a crosswalk lists related pairs only.

Every format has these columns, in this order: `source_framework`, `source_id`, `source_label`, `source_parent`, `source_parent_label`, `source_context`, `source_title`, `source_text`, `relationship`, `confidence`, `direction`, `hub_id`, `hub_label`, `paths_supporting`, `paths_total`, `target_framework`, `target_id`, `target_label`, `target_parent`, `target_parent_label`, `target_context`, `target_title`, `target_text`, `jobs_judged`, `jobs_agreeing`, `other_verdicts`, `review`, `latest_job`, `source_node_id`, `target_node_id`.

- `*_label` columns hold the label the catalog publishes for a control (`source_parent_label` and `target_parent_label` for its parent), empty when it publishes none; `hub_label` is positional with `hub_id` (one label per hub control, `; `-joined, a hub control with no label leaves its slot empty) and is empty on rows that are not derived. Labels pass through the same spreadsheet and Markdown neutralising as every other cell.
- `relationship` is read from source to target (every word reads from the source to the target: `equivalent`, `subset of`, `superset of`, `is advanced by` (the target advances the source), `complements`, `partial`, `conflicts with`; a derived row whose composition has no named type reads `related (derived, type unknown)`). `direction` is `direct`, `derived` (through the hub controls in `hub_id`) or `reverse` (a target-to-source verdict with no named inverse, reported as stored).
- `source_title` and `target_title` are the control's own title and are empty when it has none (a sub-item such as `pl-2_smt.a.13` usually has none); a title is never borrowed from another control. `source_context` and `target_context` give the control's titled ancestors, root first, as `<title> (<label>)`, else `<title> (<identifier>)` (just `<title>` for an ancestor with neither, such as an ID-less group) joined by ` > ` (for example `Planning (PL) > System Security and Privacy Plans (PL-2)`); untitled ancestors are left out, and the cell is empty when no ancestor has a title.
- `review` flags verdicts a reviewer should check; it is empty when nothing is flagged and several notes are joined by `; `. A note only reports: the verdict, confidence and row are unchanged. The kinds are `one of <n> 'equivalent' targets for <id>: <ids>` (and `... sources for <id>: ...`), on direct rows, when one control is `equivalent` to several controls of the other catalog; `paths disagree: <relationship> via <hub id>, <relationship> via <hub id>`, on a derived row whose paths compose to different named relationships; and `'<relationship>' rests on <k> of <n> paths`, on a derived row whose named relationship is supported by fewer than half of its paths. When the job ran the critique stage, a kept verdict some critics rejected adds `critique split: <r> of <n> rejected — <model> s<N>: <first reject reason>`, one the critics rejected without veto adds `critique rejected: <r> of <n> rejected — <model> s<N>: <first reject reason>`, and one whose critique failed adds `critique failed: <f> of <n> votes invalid`; a vetoed verdict has no row. Notes are computed over the whole result, so every page of a large export agrees.
- A `--via` crosswalk has one `derived` row per source and target control pair, however many hub controls link them. `hub_id` lists every hub control on the pair's paths, sorted and joined by `; `. `paths_supporting` is the number of those paths whose composed relationship is the row's (all of them when the row reads `related (derived, type unknown)`) and `paths_total` the number of all the pair's paths, so `1` of `4` marks a weakly supported inference; both are `0` on rows that are not derived. The row's `relationship` is the one named relationship its paths agree on, ignoring paths that compose to no named type; `related (derived, type unknown)` when none do, and also when two paths give different named relationships (the `review` note says which).
- `jobs_judged` counts the completed jobs that judged the pair (a bidirectional job counts once), `jobs_agreeing` those whose verdict equals the row's once read source to target, and `other_verdicts` lists every other verdict, newest first, as `<word>@<confidence> (<job>)`. `latest_job` is the job that decided the row: the newest job's verdict. If one bidirectional job's two directions disagree, its higher-confidence verdict decides (then the greater relationship type) and the other direction appears in `other_verdicts`. On a `derived` row, `confidence` is the highest supporting path's, a path's being the product of its two legs' confidences; `jobs_judged` is the number of distinct jobs across every leg of every path, `jobs_agreeing` the distinct jobs across the legs of the supporting paths, and `latest_job` the newest deciding job among the supporting paths; `other_verdicts` is empty.
- Rows are grouped by source control, then target. Each side (sources, targets) is ordered by the controls' published sort-id only when every control on that side in the result has one, else naturally by identifier (digit runs compare numerically). `--min-confidence` and `--type` filter before that choice, so they can change which order a side uses.
- `--format csv` (default) starts with a UTF-8 byte order mark so spreadsheets read the encoding. Line breaks stay inside the quoted cell; terminal escapes and other control characters, bidirectional overrides and isolates, and tag characters are removed, while script joiners (ZWJ, ZWNJ) and other format characters are kept. A cell that would start with `=`, `+`, `-` or `@` gets a leading `'` so it is not run as a formula.
- `--format xlsx` writes an Excel workbook (needs `--output <file>.xlsx`: it is binary, so without `--output` the command refuses and says so). Sheet `crosswalk` has every row with the same columns, a frozen header and a filter; `provenance` lists the mode, stage, row count, skipped mappings, server version, generation time, catalogs, contributing job IDs (`job_id`) and selected job IDs (`selected_job_id`) as field/value rows; `disagreements` has the rows where `jobs_agreeing` is below `jobs_judged`, the ones to review first. Every cell is text, never a formula, and gets the same leading `'` rule as CSV. Cells are cleaned and keep line breaks as in CSV, and a cell is capped at the 32,767 characters a sheet cell holds: longer text is cut and ends with `…`.
- `--format md` writes a Markdown table with these columns and no provenance (use JSON or XLSX for that). Cell text is cleaned like CSV (terminal escapes and other control characters, bidirectional overrides and isolates, and tag characters are removed; script joiners are kept), then `&`, `<` and `>` are written as HTML entities and `\ [ ] ( ) ! * _` and the backtick are backslash-escaped, so uploaded text cannot form HTML, links, images or emphasis; `|` is written `\|`, and each line break is written `<br>` so a row stays on one line. Text over 200 characters is cut and ends with `…`; use JSON for the text exactly as stored (XLSX cuts a cell at 32,767 characters).
- `--format json` writes `{"provenance": {...}, "rows": [...]}`. Row keys are the column names; `confidence` (two decimals), `paths_supporting`, `paths_total`, `jobs_judged` and `jobs_agreeing` are numbers. `provenance` records the mode, stage, catalogs (with whether their names are job snapshots), `job_ids` (the completed jobs that judged at least one row of the result, after `--min-confidence` and `--type`; for a derived row, the jobs of every leg of every path), `selected_job_ids` (every completed job the request selected, a superset of `job_ids`: a job that was selected but produced no row appears only here), row count, mappings skipped because a control is gone, server version and generation time, so a result can be traced to its inputs.
- `--output <file>` writes to a file instead of stdout. stderr reports, for example, `184 rows from 1 job (2 selected)`: the rows came from one job and two completed jobs were selected (`job` is `jobs` unless the count is 1); when no completed job was selected for the catalogs the crosswalk is empty and stderr says which `crosscodex run start` to run, and when jobs exist but no row remains stderr suggests `--include-non-viable` and `--stage analyzer`. Large exports are read in pages of 1000 rows; if a job completes or is removed while paging, the command fails with "the crosswalk changed while paging" and you re-run it, so a file never mixes two job sets.

Crosswalks are read from the graph, which embedded mode does not run: connect to a `crosscodexd` daemon with `--endpoint <host:port>`, or the command exits with that advice.

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
        ingestion["Ingestion Service<br/>Python + Docling<br/>separate repo"]
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
    docs -.-> ingestion -.-> catalog
    catalog --> analysis --> llm
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
    class catalog,analysis,llm sysA
    class synthesis,pipeline sysB
    class graphdb,vectordb sysC
    class tenant,tls,authn,telemetry,attestation sysD
    class docs,ingestion sysF
```

### Service Responsibilities

| Service             | Purpose                                         | Technology          |
|---------------------|-------------------------------------------------|---------------------|
| **Ingestion**       | Docling conversion; optional, own repo          | Python gRPC service |
| **Catalog**         | OSCAL parsing, document structuring, validation | Go                  |
| **Analysis Engine** | Host for analyzer plugins, DAG execution        | Go                  |
| **LLM Workers**     | Horizontally scalable LLM task execution        | Go                  |
| **Synthesis**       | Ranking, viability weighting, quality metrics   | Go                  |
| **Graph**           | openCypher queries via Apache AGE on PostgreSQL | Go                  |
| **Pipeline**        | Job orchestration, state tracking, retry logic  | Go                  |

The Ingestion service lives in its own repository, [complytime-labs/crosscodex-ingestion](https://github.com/complytime-labs/crosscodex-ingestion). When `ingestion.address` is set, the gateway sends every upload except OSCAL JSON to it and the Catalog service structures the result into controls; without it, uploads are stored unchanged and the Catalog service imports only OSCAL JSON. See [Document ingestion](#document-ingestion).

### Deployment Modes

- **Embedded** -- All services in one process with auto-bootstrapped mTLS. Requires PostgreSQL with AGE and pgvector extensions (start with `task dev:up`). Local filesystem for object storage. Catalog import, list, and inspect work for OSCAL JSON documents. Analysis does not run in embedded mode: `crosscodex run start` fails with an error that says to start `crosscodexd` and set `cli.endpoint`, and no job is stored. Run `crosscodexd migrate` before starting `crosscodexd` against a database. `crosscodexd` (default `--role=all`) bootstraps a DB pool and NATS client and runs the graph, worker, and gateway/pipeline roles together in one process; analysis result persistence is durable end-to-end at the library layer (`internal/pipeline`, `pkg/analyzer`).
- **Production (compose)** -- Single-host production stack with full mutual TLS across all services (PostgreSQL, NATS, the LLM gateway, CrossCodex gateway). One-command bring-up via `task deploy:up` after populating `deploy/.env` and generating certificates with `task deploy:certs`. The default LiteLLM gateway runs behind a stunnel sidecar to provide end-to-end mTLS; `LLM_GATEWAY=praxis` selects Praxis AI instead (spike). Every container drops all Linux capabilities (one-shot `backup-init` keeps the four it needs), runs with no-new-privileges and memory and pids limits, and all but `db-restore` have a read-only root filesystem. See [deploy/README.md](deploy/README.md) for setup, first-use CLI commands, and operations.
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

The embedded daemon signs an attestation of every catalog import (see [Cryptographic Attestation](#cryptographic-attestation)). With `attestation.enabled: true` (the default) and neither `attestation.private_key_path` nor `attestation.public_key_path` set, it uses `pki/attestation-key.pem` and `pki/attestation-pub.pem`, which a generated set includes. A set you supply has no key pair, so either put an ECDSA key pair at those two paths, set both key paths, or set `attestation.enabled: false`; otherwise the daemon refuses to start and names the missing file.

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

# Storage (crosscodexd only: embedded mode always stores objects under its data directory)
storage:
  objects:
    backend: local                # local | s3; each tenant's objects go under its own <tenant>/ directory or key prefix
    base_path: /var/lib/crosscodex/objects  # local: required by crosscodexd, objects at <base_path>/<tenant>/
    bucket: ""                    # s3: required; objects at s3://<bucket>/<tenant>/
    region: ""                    # s3: optional; the AWS SDK's default region otherwise
    endpoint: ""                  # s3: optional; an S3-compatible server (path-style), e.g. MinIO

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

# Attestation (roles all, pipeline and gateway, and the embedded daemon, refuse to start if an enabled scope's keys are missing or do not load)
attestation:
  enabled: true                   # false: jobs and catalog imports complete unattested (jobs: logged at startup)
  private_key_path: /etc/crosscodex/attestation-key.pem   # ECDSA (SEC1 "EC PRIVATE KEY")
  public_key_path: /etc/crosscodex/attestation-pub.pem    # PKIX "PUBLIC KEY" of the same key
  expiry_duration: 8760h          # layout validity; `results verify` rejects an expired layout
  storage_retries: 3              # retries of a failed attestation write, [0, 10]; global-only
  storage_retry_delay: 2s         # wait before each retry, [0, 1m]; global-only
  tenant_overrides:
    sandbox:
      enabled: false              # this tenant's jobs and imports run unattested

# X.509 client certificate -> tenant and roles. Required by roles all and
# gateway: the first mapping a certificate matches decides; a certificate that
# matches none is refused. Roles: admin, reviewer. Match fields are globs
# (cn, organization, org_unit, san_email, san_dns, san_uri), ANDed.
tenants:
  enabled: true                   # false: omit tenant (or set tenants.default_tenant); every caller is in tenants.default_tenant
auth:
  x509_mappings:
    - match:
        cn: "ops-admin"
        organization: "Acme"
        org_unit: "Engineering"
      tenant: acme-engineering
      roles: [admin]
    # A reviewer gets a certificate of their own: admin does not include
    # reviewer, and one certificate holding both would let one person apply a
    # cleaning pass and decide its findings.
    - match:
        san_email: "alice@acme.example"
      tenant: acme-engineering
      roles: [reviewer]

# Analysis
analysis:
  engine:
    task_timeout: 5m                     # (0, 30m]; fail a stage when neither its own results nor any worker progress arrive for this long
    success_timeout: 10m                 # (0, 24h], >= task_timeout, <= pipeline.stage_timeout; fail a stage (llm_unavailable) that receives only failed results for this long
    progress_flush_interval: 10s         # (0, 1h]; how often a running stage's task progress is saved (global only)
    max_retries: 3                       # Redispatches of a failed task
    retry_backoff: 1s                    # Base delay before a redispatch
    retry_on_timeout: false              # Re-send a timed-out LLM call (it may already be billed); tenant-overridable via tenant_overrides
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
    models: []                           # Panel models, each a different alias; required when enabled (deploy/config.yaml: panel-1..3)
    allow_shared_panel_models: false     # Run although the panel has one alias, two aliases answer as one upstream model, or one's model is unknown; for single-model and offline stacks (global-only)
    confidence_weights:                  # Weight of each vote's reported CONFIDENCE in a mapping's confidence; each (0, 1], high >= medium >= low (global-only)
      high: 1.0
      medium: 0.67
      low: 0.33                          # Also used for a vote that reports no CONFIDENCE
  requires:
    enabled: false                       # Off in the compiled defaults (deploy/config.yaml turns it on); needs embedding and an enabled candidates generator; finds REQUIRES edges, which are not mappings
    models: []                           # Panel models, each a different alias; required when enabled (deploy/config.yaml: panel-1..3)
    allow_shared_panel_models: false     # As under relationship (global-only)
    samples_per_model: 1                 # Votes per model per pair; odd unless allow_even_samples
    allow_even_samples: false            # Allow an even samples_per_model
    sampling_temperature: 0.3            # [0, 2]
    max_tokens: 300                      # Max tokens per LLM reply
    consensus_threshold: 0.66            # [0.5, 1]; minimum confidence fraction
    max_error_rate: 0.34                 # [0, 1]; maximum fraction of votes that can fail
    max_source_chars: 1500               # Max runes of source control text
    max_target_chars: 800                # Max runes of target control text
  critique:                              # Re-reads each verdict relationship and requires accepted and flags (or, with veto, drops) the ones it rejects (#260)
    enabled: true                        # On by default; registered only while relationship or requires is enabled. false makes their verdicts final
    models: [panel-1, panel-2, panel-3]  # Panel aliases, each different; required when enabled
    allow_shared_panel_models: false     # As under relationship (global-only)
    samples_per_model: 1                 # Votes per model per pair; odd unless allow_even_samples
    allow_even_samples: false            # Allow an even samples_per_model
    sampling_temperature: 0.3            # [0, 2]
    max_tokens: 300                      # Max tokens per LLM reply
    consensus_threshold: 0.66            # [0.5, 1]; a verdict is rejected when REJECT votes / valid votes reach it
    veto: false                          # false keeps a rejected verdict, flagged with outcome "rejected"; true drops it (vetoed). Global-only
    max_error_rate: 0.34                 # [0, 1]; more failed votes than this fails the critique, which keeps the verdict
    max_source_chars: 1500               # Max runes of source control text
    max_target_chars: 800                # Max runes of target control text
  candidates:
    min_embedding_coverage: 0.8          # Minimum fraction of a job's non-section controls that have a statement and must have an embedding before candidate generation runs; below this, the candidate_generation stage fails the job (fail-closed)
    embed_model: "snowflake-arctic-embed2"  # Embedding model candidate generation reads; must be one of analysis.embedding.models
    max_per_source: 10                   # Candidate pairs kept per source control after rank fusion, and so the pairs each pair analyzer (requires, relationship) analyses per source; positive integer (default 10); each enabled semantic or lexical top_k must not exceed it
    generators:                          # Candidate generators; none by default, and with none enabled no candidate pairs are produced. Enabled generators are fused per source: each ranks its targets by score (best is 1; tied scores share the mean of the positions they span), a target's fused score is the sum of weight / (60 + rank) over the generators that found it, and the top max_per_source by fused score are kept (ties broken by target ID, ascending)
      - name: semantic                   # Most-similar controls by embedding
        enabled: true
        weight: 1.0                      # [0, 1] in rank fusion; 0 or unset means 1.0
        config:                          # Optional; unknown keys are rejected at load
          top_k: 10                      # Most-similar targets kept per source control; positive integer (default 10)
          min_similarity: 0              # Drop targets below this cosine similarity x 100; [0, 100] (default 0)
      - name: lexical                    # Targets ranked by BM25 over control text
        enabled: false
        weight: 1.0
        config:
          top_k: 10                      # Most-similar targets kept per source control; positive integer (default 10)
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
  max_in_flight: 1                        # [1, 256]; tasks each worker runs at once; match the model server's parallel slots, and keep llm.timeout covering the time a request queues there; cuts wall time, not LLM calls; per worker host (global-only)

pipeline:
  stage_timeout: 24h                      # (0, 168h]; hard cap on each analyzer stage, which worker progress never extends; fails a stage whose task was lost; worker hosts must set the same or a larger value (they keep abandons that long)
  graph_materialize_timeout: 30m          # (0, stage_timeout]; how long a job's graph stage waits for a graph role to confirm the job's graph is written before failing the job (global-only)

graph:
  max_bulk_edges: 1000                    # Most edges per BulkCreateEdges request, range [1, 10000]; larger requests are rejected (global-only)
  max_similarity_catalogs: 100            # Most catalogs per SimilaritySearch request, range [1, 10000]; requests naming more are rejected (global-only)
  max_similarity_limit: 1000              # Largest per-catalog limit a SimilaritySearch request may ask for, range [1, 10000]; larger limits are rejected (global-only)
  addr: ""                               # host:port the standalone graph role listens on for the gateway (mutual TLS, target graph-server); required by role graph, empty otherwise (global-only)
  endpoint: ""                            # host:port the gateway role dials to reach the graph role (mutual TLS, target graph-client); required by role gateway, empty otherwise (global-only)

ingestion:                                # Document conversion service; see "Document ingestion"
  address: ""                             # host:port; empty = not configured (global-only)
  poll_interval: 2s                       # > 0 (global-only)
  convert_timeout: 10m                    # >= poll_interval (global-only)
  max_document_bytes: 52428800            # > 0; 50 MiB (global-only)

catalog:
  requirement_part_names: [statement]     # Ordered OSCAL part names that hold a control's requirement text; the first a control has wins; at least one, each an OSCAL token (global-only)
  structuring:
    print_artifact_patterns: []           # RE2 (multiline) patterns removed from controls structured from a document, never from OSCAL imports; default [] (none); a pattern matching empty text is rejected (global-only). See "Print-artefact patterns"

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

#### Task retries

The collector retries a failed LLM task by the result code the worker reports. A rate limit that outlasts the gateway's own retries, and any other failure, are re-sent after `analysis.engine.retry_backoff` (doubling each attempt), at most `analysis.engine.max_retries` times, so one task makes at most (1 + `analysis.engine.max_retries`) × A upstream calls, where A is `1 + llm.max_retries` when `llm.gateway_mode` is false (the default), and otherwise 1 + the retries the gateway itself is configured with (the client makes one attempt per call and ignores `llm.max_retries`, as `deploy/config.yaml` sets `gateway_mode: true`). A task that timed out (the client's deadline, or HTTP 408 or 504 from the gateway or provider) is sent once unless `analysis.engine.retry_on_timeout` is true, because the provider may already have billed the call. A truncated answer (`max_tokens` too small) and a malformed payload are never re-sent: the same request fails the same way. `analysis.tasks.retried` counts re-sends by `cause` (`timeout`, `rate_limit`, `other`).

#### Print-artefact patterns

Documents converted from PDF can carry a publisher's running headers, footers and file paths in their text. `catalog.structuring.print_artifact_patterns` removes them from controls structured from a document. It never touches OSCAL imports or search queries, and none are on by default. Each entry is an RE2 pattern compiled with the multiline flag, so `^` and `$` match at line boundaries. End a pattern with `$\n?` to remove a whole line with its line break, or leave out the anchors to remove a span within a line. Keep patterns specific: a pattern that matches ordinary prose removes it, unless it would leave the control's text blank, in which case the text is kept whole.

```yaml
catalog:
  structuring:
    print_artifact_patterns:
      # US GPO (Federal Register, CFR) page header, on one line
      - '^.*\bVerDate\b.*\bJkt\s+\d+.*\bPO\s+\d{5}.*\bFrm\s+\d+.*\bFmt\s+\d+.*\bSfmt\s+\d+.*$\n?'
      # US Code compilation file path
      - '^[ \t]*G:\\COMP\\.*$\n?'
      # A German page footer such as "Seite 3 von 12"
      - '^[ \t]*Seite \d+ von \d+[ \t]*$\n?'
      # A draft marker anywhere in a line
      - ' ?\[DRAFT\]'
```

A header the extractor wraps across lines does not match a one-line pattern; write the pattern for the layout your documents have.

#### Document ingestion

The `ingestion` block points the gateway at the [crosscodex-ingestion](https://github.com/complytime-labs/crosscodex-ingestion) service, which converts PDF, HTML, DOCX and text to structured JSON.

```yaml
ingestion:
  address: ""                             # host:port of the ingestion service, for example ingestion.internal:8090; empty (default) = not configured, so documents that are not OSCAL JSON are rejected as before (global-only)
  poll_interval: 2s                       # > 0; how often a running conversion is checked (global-only)
  convert_timeout: 10m                    # >= poll_interval; longest wait for one document (global-only)
  max_document_bytes: 52428800            # > 0; largest document sent for conversion, 50 MiB by default; larger is refused before any call (global-only)
```

All four fields are global-only (no `tenant_overrides`): the service and its capacity are shared by every tenant. The connection follows `tls.targets.ingestion` merged over the global `tls` settings; a resolved mode of `off` dials plaintext (dev and tests only). `CROSSCODEX_INGESTION_ADDRESS` sets the address. A bad value fails config loading with a message naming the field.

- **Routing.** OSCAL JSON of any OSCAL model is stored unchanged, as before; every other document goes to ingestion. Known limitation: with ingestion configured, OSCAL XML and YAML also go to ingestion, so the advice to convert them with `oscal-cli` is no longer shown. Convert them to JSON first.
- **Shared storage.** Ingestion writes its output to storage the gateway also reads: the same directory at the same absolute path (`storage.objects.base_path`, which must exist) or the same S3 bucket (`storage.objects.bucket`). The gateway accepts only output inside the calling tenant's own path and refuses any other URI. The embedded daemon shares its data directory.
- **Structuring.** Controls are found by the heading, table and numbering rules, or by `catalog.structuring.section_pattern` when set. No LLM is wired for structuring, so a document those rules cannot split is refused and no catalog is stored. `catalog.structuring.section_pattern`, `decompose` (default `true`), `min_decompose_words`, `filter_by_keywords`, `keywords`, `max_heading_repeats` and `print_artifact_patterns` apply; `chunk_chars` affects only the LLM tiers and `max_validation_chars` and `allowed_formats` are not read.
- **Provenance.** A stored catalog's `source_uri` is the converted document's storage key, and its extractor is `crosscodex-ingestion` at the output's `schema_version`.
- **Known limitation: no groups.** Imported documents currently produce titled controls but no group containers, so a catalog's hierarchy is flat.
- **Errors.** The ingestion service's error text is logged on the server and never returned. The caller gets a generic message with an error reference, to give to an operator, who finds the cause in the server log.
- **Known limitation: orphaned output.** A conversion that fails, times out or is cancelled can leave files in the shared storage that CrossCodex does not remove. When the converted document is refused (`InvalidArgument`, `FailedPrecondition`) the gateway deletes the converted output, but when structuring or storing it fails with an internal error the output stays in the tenant's storage. With its attestation enabled, the ingestion service also writes a second object per conversion (`attestation.link`, next to the output), which CrossCodex never deletes. The ingestion service has no cancel or delete call, so CrossCodex cannot reclaim any of these; clear them from storage when it matters.
- **Cost.** A conversion can take minutes on CPU; `ingestion.convert_timeout` and `ingestion.max_document_bytes` bound it, and `crosscodex.ingestion.conversion.duration_ms` shows how long conversions take. No LLM calls are made.

To try it locally, `task deploy:local INGESTION=1` (or `INGESTION=rustfs`) starts the service; see [deploy/README.md](deploy/README.md#document-ingestion-ingestion). How it works is in the [Service Runtime Guide](docs/dev/service-runtime.md#document-ingestion).

#### Analyzer `enabled` settings

`analysis.classification.enabled`, `analysis.embedding.enabled`, `analysis.artifacts.enabled`, `analysis.requires.enabled` and `analysis.relationship.enabled` decide which analyzers a job runs. A disabled analyzer gets no job stage, never runs and makes no LLM calls. The defaults enable classification and embedding only, so a stock deployment classifies and embeds controls but produces **no mappings**: mappings come from the relationship analyzer, which is off until you set `analysis.relationship.enabled: true`, its `models`, and at least one enabled `analysis.candidates.generators` entry. The compose stack's `deploy/config.yaml` turns mapping on: it enables `relationship` and `requires`, each asking the gateway aliases `panel-1..3` once, with the `semantic` generator (see [deploy/README.md](deploy/README.md#what-an-analysis-runs-and-what-it-costs)). `crosscodex run status <job-id>` lists the analyzers a job runs and says when it has no relationship analyzer.

`analysis.critique.enabled` is the exception to "off until you enable it": it defaults to `true` but registers the `critique` stage only while `relationship` or `requires` is enabled (the daemon logs that it did not register it otherwise), so a stock deployment is unchanged. Turning it on or off therefore changes a job's analyzer set, and a job created under the other setting fails on resume as described below.

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

See [Sources of Test Material](docs/dev/test-sources.md) for where the catalogs, PDFs and HTML that evals and smoke runs use come from.

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

### Evaluating crosswalk quality

`task eval:crosswalk` measures how well a deployment's crosswalk analysis finds the cross-framework references that an OSCAL catalog itself cites (for example, the SP 800-53 controls SSDF lists for each of its tasks). It runs a small, fixed set of source controls through a gateway you already run, so its cost is set by the profile and by the size of the catalogs, as the cost notes under Limits explain. It is for tuning models, prompts and retrieval settings; it is not a CI gate.

```bash
task deploy:smoke:up OPENAI_COMPAT_HOST=<host:port>
task eval:crosswalk PROFILE=ssdf-to-sp800-53r5 LABEL=baseline
task eval:crosswalk PROFILE=ssdf-to-sp800-53r5 LABEL=new \
  BASELINE=.test-output/eval/crosswalk/ssdf-to-sp800-53r5/baseline/report.json
task deploy:smoke:down
```

Requires `jq` on the host (the runner reads job status with it). Variables: `PROFILE` (required), `LABEL` (default: UTC timestamp), `BASELINE` (an earlier `report.json`), `MODE=llm` (print only the summary line and errors), `NO_CACHE=1`, `JOB=<id>` (score an existing job instead of starting one; refused unless its catalogs are byte-identical to the profile's subset and target, see the [package README](test/eval/crosswalk/README.md#scoring-an-existing-job)), `EVAL_TIMEOUT` and `EVAL_POLL` (seconds, defaults 86400 and 10; a mapping run on a local model takes hours). `EVAL_ENDPOINT`, `EVAL_CA`, `EVAL_CERT` and `EVAL_KEY` default to the `deploy:smoke:up` stack; set them to evaluate another gateway. The model, panel and retrieval come from the daemon's configuration; the eval has no model settings.

Shipped profiles (references are the resolved pairs the score uses):

| Profile | Source controls | References |
|---|---|---|
| `ssdf-to-sp800-53r5` | 10 SSDF tasks, every practice group | 36 |
| `sp800-171r3-to-sp800-53r5` | 10 SP 800-171r3 requirements, 8 families | 28 |

Each run writes `.test-output/eval/crosswalk/<profile>/<LABEL>/` with `report.md` (what the task prints), `report.json` and the raw exports. The report names the job, the catalog IDs, SHA-256 hashes of the profile and catalogs, and the tool's git commit (`-dirty` when it was built from a tree with uncommitted changes), plus each model's deployment hash and, for a profile that names a `references.csv`, that file's hash; each reference carries its citation. A run directory is never overwritten: reuse of a `LABEL` is an error.

Limits to keep in mind:

- The references are what a framework chose to cite, which is sparse. The report measures recall (how many cited pairs the analysis found or could reach) and is not proof of precision: a pair absent from the references may still be correct. Its Precision section reports the share of produced rows that share a base control with a reference row for the same source (the recall rule, in both directions) as a lower bound, and, when `test/eval/crosswalk/labels/<profile>.yaml` exists, the share of labelled rows judged related and how much of the output the labels cover (see the package README, "Labels").
- "Found exact" counts a reference when the analysis related its source control to the referenced target control, where a statement item the importer derives from a control (for example `sa-15_smt.b` from `sa-15`) counts for the control that contains it, on both sides. "Base" also credits a mapping to a control the profile's `base_level` rolls up to the same base as the referenced one: `root` (default) the outermost control of its tree, so an enhancement and its base control match; `parent` its immediate parent control; `none` no roll-up, so Base equals Exact. The report prints the level beside the Base figures. The source subset keeps each listed control's ancestor controls (with their params) as context, so the importer analyses them too and the run costs slightly more; only the listed controls are scored. Candidates are scored the same way as "reachable".
- The summary line and the report count chat calls only (`chat calls`: classify, relationship and requires), taken from `run status`; embedding calls are listed per analyzer in the report table and left out of that total. A figure is shown only when every analyzer's cache summary is attested and was run with the cache enabled or bypassed.
- Cost is not bounded by the profile; it has two parts. Figures are as measured on these runs, not guarantees. (1) Classify and embedding run over every control or item of both catalogs, so on a cold cache they dominate and do not depend on the profile: SP 800-53r5 plus the subset gave 2,290 classify tasks, 1,740 dispatched in the first SSDF run. The result cache keeps those results, so later runs mostly reuse them (the SP 800-171r3 run dispatched 44 of 2,339). (2) Relationship and requires tasks per analyzer are candidate pairs times the panel size (3), and candidate pairs are the decomposed source items times candidates per item, so the number of controls alone does not predict them. SSDF: 10 controls, 10 source items with candidates, 100 candidates, 300 tasks per analyzer. SP 800-171r3: 10 controls, 46 requirement items with candidates, 460 candidates, 1,380 tasks per analyzer (the imported subset has 59 rows, which include the 10 parent sections and other non-requirement rows that the analyzers skip). The chat calls the report counts are classify plus relationship plus requires dispatched (embedding excluded): 2,340 for SSDF and 2,804 for SP 800-171r3. Wall time on qwen3.8-flash-next behind an OpenAI-compatible server was about 3,650 s cold for SSDF and about 10,350 s for SP 800-171r3, which is relationship-bound (relationship and requires took about 10,250 s each).
- The result cache is on by default, because a crosswalk run otherwise re-sends thousands of classification calls that have nothing to do with the mappings being measured. A repeat of an unchanged configuration therefore reuses earlier relationship results; the report states how many, and agreement with a baseline is then labelled "not independent" because the two runs share answers. Use `NO_CACHE=1` (passes `--no-cache` to `run start`) to measure run-to-run variance, at the full chat-call cost.

To add a profile, see [test/eval/crosswalk/README.md](./test/eval/crosswalk/README.md).

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

When attestation is enabled for a job's tenant (`attestation.enabled`, `attestation.tenant_overrides`), the pipeline records an in-toto attestation of the job in object storage under `attestation/<job-id>/` in the tenant's object store (for the `local` backend, `<storage.objects.base_path>/<tenant>/attestation/<job-id>/`), the prefix retention's `attestation` class collects:

- **Layout** (`layout.json`): signed before any analyzer runs; one step per enabled analyzer, then `synthesis` and `graph`.
- **Links** (`links/<step>.json`): one signed link per step. Every link records the job, `job:<tenant>/<job-id>`, with the SHA-256 of the job's stored config, and every layout step expects it, so an attestation cannot be passed off as another job's. An analyzer's link records the SHA-256 of its stored result and of the results it consumed; synthesis records every analyzer's result and its own output (the SHA-256 of the JSON of the result it computed, not of rows an auditor can read back from the database); graph records the synthesis output. `candidate_generation` has no link.
- **Bundle** (`bundle.json`): the layout and every link, stored only after the chain verifies. The job is `COMPLETED` only once it is stored.

The job fails instead of completing unattested. Every error names the step and object:

| Failure | Job | What to do |
|---|---|---|
| Layout or a link cannot be signed | Fails (the step's stage too, for synthesis and graph); a layout failure fails it before any analyzer runs | Fix the key pair and restart `crosscodexd`, then `crosscodex run retry --from-failure <job-id>` or start a new job. If you fixed it by rotating to a new key pair, start a new job: the links the job already stored were signed with the old key and no longer verify, so a retry fails |
| Object storage refuses a write after `attestation.storage_retries` retries | Fails | Fix storage, then `crosscodex run retry --from-failure <job-id>` or start a new job |
| A stored link is missing, does not verify, or is for another job on resume, or the chain does not verify | Fails, with every stored object left as it was; `run retry --from-failure` refuses it | Start a new job; check whether objects under `attestation/` were changed |

Publishing the bundle to the audit stream waits for the stream to store it: if it does not, the job fails (the stored bundle is kept, and `crosscodex run retry --from-failure` finalizes again). When attestation is disabled, no signer is built, no attestation object is written, and the daemon logs at startup that jobs are unattested.

Every catalog import (`crosscodex catalog import`, RPCs `SubmitDocument` and `StreamDocument`) of a tenant with attestation enabled is attested too, by the gateway (roles `all` and `gateway`) or the embedded daemon, with the same key pair as that tenant's jobs. So roles `gateway` and `all`, not only `pipeline`, need `attestation.private_key_path` and `attestation.public_key_path` whenever `attestation.enabled` is true (or set `attestation.enabled: false`); otherwise they refuse to start. It stores one signed in-toto link at `attestation/ingest/<document-id>` in the tenant's object store, where `<document-id>` is the `document_id` the import returns, the uploaded document's own storage key (today `documents/<uuid>.json`, so the link is `attestation/ingest/documents/<uuid>.json`). The link's step is `gateway.SubmitDocument` or `gateway.StreamDocument`; its material is `document://<tenant>/<document-id>` with the SHA-256 of the uploaded bytes as its digest; its product is `catalog://<tenant>/<catalog-id>`; its byproducts are `content_hash` (the same SHA-256), `trace_id`, `catalog_format` and `catalog_name` (always present; `attestation.include_byproducts: false` only drops the generator's `span_id`, `timestamp` and `hostname`). If the link cannot be signed or stored, the import fails with `Unavailable`: the catalog was imported but not attested, and the message names the catalog and an error reference the daemon logged the cause under. Re-run the same import to attest it: the catalog ID derives from the content, so the catalog is updated in place.

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
archived as `{catalog, classifications, controls, containers}` and then purged together with
its `classifications`, `controls`, `catalog_containers`, and `embeddings`. The archive deliberately
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

### Mapping Feedback (`crosscodex results`)

Reviewers vote on a completed job's mappings and, when a mapping is wrong, determine it invalid. A mapping is one synthesized verdict of the job; its `mapping_id` is the ID `results export` prints. The commands need the `reviewer` role (see [Key Configuration Examples](#key-configuration-examples)) and a completed job.

```sh
# List the job's mappings that need review. --filter is contested (the default: escalated,
# highly disputed or low-confidence), low-confidence, high-disagreement, escalated or unvoted.
crosscodex results review <job-id> --filter unvoted --page-size 20

# Vote: --vote is agree, disagree, refine or escalate. refine needs --suggest <relationship type>.
crosscodex results vote <job-id> <mapping-id> --vote refine --suggest SUBSET_OF \
  --rationale 'target is narrower' --evidence source=audit-2026

# Decide. Exactly one of --valid or --invalid, with a --rationale.
crosscodex results determine <job-id> <mapping-id> --invalid --rationale 'different scope' --evidence ticket=SEC-17
```

- **Votes are signals.** A reviewer's latest vote on a mapping is the one counted. `agreement_score` is agree votes over counted votes. A mapping `needs_escalation` with any `escalate` vote, or with at least one vote and an agreement score below `feedback.min_agreement`. `--rationale` is required except for `agree` (at most 4096 bytes); `--evidence key=value` may repeat (at most 32 entries).
- **The queue.** Items are ordered by (source, target). `review` prints the next page's command when more remain; `--page-size` is 1 to 1000 (default 50). `--filter` `low-confidence` lists mappings below `feedback.low_confidence`; `high-disagreement`, voted mappings below `feedback.min_agreement`; `unvoted`, mappings with no vote. Each item names why it is listed.
- **Determinations decide.** Votes alone never change a mapping. The rationale is required and permanent; the determination is signed (a `feedback.determination` in-toto link, which needs `attestation.private_key_path` and `attestation.public_key_path`) and recorded, then applied to the graph. `determine` prints the status and whether the graph changed. The caller must be an active registered reviewer (`crosscodexd admin reviewer add`), as for cleaning decisions.

| Review now | `--invalid` | `--valid` |
|---|---|---|
| none, `valid`, `stale` | retires the mapping's live edges; refused when it has none (cleaning retired it, or its job's edges are gone) | recorded; no graph change |
| `invalid` | refused: already invalid, naming the determination | restores the retired edges; refused when a graph rebuild removed them (run `crosscodexd admin cleaning --reproject` first) |

A valid determination on an already restored mapping stores no edge IDs, so `results` stops showing it as `restored`; the restore stays in the audit record. Determinations never change a mapping's relationship type: to change it, record `--invalid` and re-run the analysis.

Mapping reads are tenant-wide: a reviewer can act on a mapping of a job another user started.

Where each call works:

| Shape | Votes, queue | Determinations |
|---|---|---|
| role `all` | in process | in process |
| `gateway` + `graph` | the gateway forwards to the graph role | same |
| embedded daemon | in process | refused with `FailedPrecondition`: "mapping determinations need the crosscodexd daemon (role all, or gateway with a graph role)" |

Configuration (global only: a review threshold belongs to the deployment's review process, not to a tenant's data):

```yaml
feedback:
  min_agreement: 0.5    # [0, 1]; below this a voted mapping needs escalation and lists as high disagreement
  low_confidence: 0.5   # [0, 1]; the review queue lists a mapping below this confidence as low confidence
```

See [Mapping Feedback](docs/dev/feedback.md) for the tables, transitions, write order and errors.

### Edge Cleaning (`crosscodex admin cleaning`)

Detect redundant, contradictory and stale mapping verdicts, then review them. A verdict is one job's judgement of one control pair, written as `<job>:<source>:<target>`; a finding is what a signal raised about one or more verdicts. Retiring a verdict supersedes its graph edges (sets `valid_to`); nothing is deleted, so as-of queries still show the history. Retiring changes only the graph (the `crosscodex crosswalk` export and graph queries): `crosscodex results`, which reads `vote_summaries`, still lists retired verdicts, each marked with its cleaning state (see [Reading a job's mappings](#9-read-a-jobs-mappings)).

`crosscodexd` serves `EdgeCleaningService` in role `all`, and in role `gateway`, which forwards it to the `graph` role (see "Upgrading"). The embedded daemon fails the commands with `Unavailable` and says edge cleaning needs the crosscodexd daemon; the daemon's other roles serve no gateway, so they have nothing to call.

```sh
# 1. Preview (changes nothing). The output ends with the exact apply command.
crosscodex admin cleaning run --tenant acme

# 2. Apply the previewed plan; the token binds the apply to that plan.
crosscodex admin cleaning run --apply --expect <token> --tenant acme

#    If the graph write of an apply or decision stopped part-way, finish it.
#    No token, no catalog scope.
crosscodex admin cleaning run --resume --tenant acme

# 3. List findings; filter with --signal, --status, --source-catalog with --target-catalog.
crosscodex admin cleaning findings --tenant acme --status pending

# 4. Read one finding. The output ends with the exact decide command and state token.
crosscodex admin cleaning show <finding-id> --tenant acme

# 5. Decide with that state token.
crosscodex admin cleaning decide <finding-id> --verdict dismiss \
  --rationale "false positive" --expect <state-token> --tenant acme
```

- **Preview and apply.** `run --apply` recomputes the plan and refuses it, writing nothing, unless its token equals the preview's. The token covers the scope, the tenant's effective cleaning policy and every planned finding with its evidence and edges, so a policy change, or a graph or findings change that alters the plan, refuses the apply: preview again. `--source-catalog` and `--target-catalog` (both or neither) limit a run to one catalog pair, in either direction; pass the same pair to the apply.
- **Unfinished graph writes.** If the graph write of an apply or decision stops part-way, the command fails with `FailedPrecondition`, saying how many findings were applied to the graph out of how many and to run `run --resume`; a failed decide also says the decision was recorded, so do not decide again. While any recorded change is not yet applied to the graph, preview, apply and decide are refused and tell you to run `run --resume`.
- **Decide.** `--verdict` is `retire`, `dismiss` or `restore`. `retire` is allowed from `reported`, `pending`, `dismissed` or `restored`; `dismiss` from `reported` or `pending`; `restore` from `retired`. A `cleared` finding takes no decision. `--retire-verdict <job>:<source>:<target>` (repeatable, `retire` only) names which of the finding's verdicts to retire; without it, `retire` removes the verdicts the signal targets, and a finding whose signal targets none (`equivalence_fanout`) needs it. A decision against a finding that changed since `show` is refused: run `show` again and use the new state token. `--rationale` is required and may not be blank.
- **Stale findings are per job.** `stale_prompt` and `stale_model` raise one finding per job, not per verdict: `show`, `findings` and `run` print the job's catalogs, when it completed and how many of its live verdicts the run read, and the detail lists each stale stage's recorded and current value. Its ID depends only on the signal and the job, so retiring or restoring one of the job's verdicts through another finding does not re-raise a dismissed one; a change to a stale stage or its values does. Under action `retire`, and for a reviewer's `retire`, it removes the job's live verdicts (all of them count against `max_retire_per_run` on a run), or only those `--retire-verdict` names, which must be among them; `restore` recreates exactly what was retired, whatever the job's size.
- **Restore.** Restoring recreates each retired edge as a new live edge with a new ID, derived from the retired edge and the restore decision; the retired edge stays superseded. See the Upgrading note on `mapping_id`.
- **Statuses.** `reported` (listed only), `pending` (in the review queue), `retired`, `dismissed`, `restored`, and `cleared` (a later pass over its catalog pair no longer detects it).
- **Roles.** `run` needs `admin`. `findings` and `show` need `reviewer` or `admin`. `decide` needs `reviewer`, which `admin` does not include, and the caller must be an active reviewer of the tenant (`crosscodexd admin reviewer add`). Grant `reviewer` with an `auth.x509_mappings` entry for the reviewer's own certificate, not the admin's (see [Key Configuration Examples](#key-configuration-examples)); a caller without it is refused with `PermissionDenied`.
- **Signing.** Apply and decide sign what they record as in-toto links with `attestation.private_key_path` and `attestation.public_key_path`, whatever `attestation.enabled` says. With either path empty, apply and decide are refused with a message naming both settings; preview, resume, `findings` and `show` still work. With both set but not loadable as a key pair, `crosscodexd` in role `all` refuses to start.
- **JSON.** `--json` prints the response with the proto field names (snake_case), including zero counts and empty lists; protojson renders 64-bit integers such as `summary.skipped_edges` as strings.

The policy lives in the `cleaning` config section. Each signal's `action` is `report` (record the finding), `human` (queue it as `pending` for a reviewer) or `retire` (retire its target verdicts on apply). Defaults:

```yaml
cleaning:
  max_retire_per_run: 1000        # at least 1; an apply that would retire more verdicts is refused
  signals:
    equivalence_fanout:           # a control EQUIVALENT to two or more controls of one other catalog
      action: report              # report or human; retire is rejected
    cross_job_contradiction:      # jobs disagree on a pair; targets every verdict but the latest job's
      action: report
    low_agreement:                # the panel's agreement_fraction is below the threshold
      action: report
      min_agreement_fraction: 0.5 # (0, 1]; valid only on low_agreement
    conflicts_with:               # a CONFLICTS_WITH verdict
      action: report
    stale_prompt:                 # a job stage's prompt version differs from the one the tenant resolves now
      action: report
    stale_model:                  # a job stage's panel aliases or llm.model_revisions changed since
      action: report
  tenant_overrides:
    acme:
      max_retire_per_run: 200
      signals:
        conflicts_with:
          action: human
```

Every field is tenant-overridable: `max_retire_per_run` and each signal's `action` and `min_agreement_fraction`. An unset override field inherits the global value, and overrides are validated like the globals. A stage that predates recording, or a prompt version unknown on either side, is never stale.

See [Edge Cleaning](docs/dev/cleaning.md) for the signal rules, tokens, write order and the audit record.

#### Re-projecting after a graph rebuild

A rebuilt graph (the rebuild runbook in the #148 upgrade note of [`docs/dev/design-principles.md`](docs/dev/design-principles.md)) holds every new job's edges live again, while the stored findings and determinations still say `retired` or `restored`. `crosscodexd admin cleaning --reproject` writes them back. It runs on the host like the other one-shots, opens the database and graph, and makes no LLM call and no NATS connection.

```sh
crosscodexd admin cleaning --reproject --tenant acme --dry-run   # preview, writes nothing
crosscodexd admin cleaning --reproject --tenant acme
crosscodexd admin cleaning --reproject --all-tenants
```

- **Preview first.** Both engines (cleaning findings, then mapping reviews) print counts, `retired N, relinked N, staled N, in_sync N`, and one line per item that is not in sync; with `--dry-run` each line starts with `would`. Without `--dry-run`, both engines first run as a dry run, and if either refuses, nothing is written.
- **Actions.** `retire`: a retired finding's or invalid review's verdict has a live edge again, so it is superseded (at the decision's time, or at the edge's `valid_from` when that is later). `relink`: a restored verdict's stored edge IDs are replaced with its live edges. `stale`: the verdict is gone, because the graph was rebuilt from new jobs. `in_sync`: nothing to do.
- **Which decision wins.** Within each engine the latest decision wins: among the findings that target a verdict, its latest retire or restore decision; for a mapping, its latest determination. The engines do not compare decisions with each other. Each replays its own latest decision, cleaning first, so when both have decided on one verdict, the verdict ends up retired if either engine's latest decision retires it.
- **Stale.** A verdict is gone, and its finding or review `stale`, only when the graph holds no `SEMANTIC_MATCH` edge of it at all (its source, target and job), live or superseded, under any edge ID. An edge the other engine retired in the same run counts as present. For a review, its original edge (ID equal to `mapping_id`) or any of its stored edge IDs also count, and the review stays `in_sync`. A finding or review whose job retention purged has no verdicts left to read, so its live edges cannot be retired. It is judged by its stored edge IDs and the graph's edges of its verdict, and is `stale` only when the graph holds none of them. A stale finding or review takes a signed record (`decided_by` is `reproject`, naming the missing verdicts) and no decision. A later cleaning pass that detects the finding again raises it as `new`. Run a new pass afterwards (`crosscodex admin cleaning run`, then `--apply`) to raise findings on the new jobs' verdicts.
- **Refusals.** It refuses, writing nothing, while a finding or determination is not yet applied to the graph (run `crosscodex admin cleaning run --resume`, or record a determination, which first applies the outstanding ones). Without `--dry-run` it needs `attestation.private_key_path` and `attestation.public_key_path`.
- **`--all-tenants`** covers active tenants only and needs [`database.tenant_admin_dsn`](#tenant-administration); a failing tenant does not stop the rest, as for the other one-shots. A second run reports every item `in_sync`.

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

crosscodexd admin cleaning --reproject --tenant acme --dry-run

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
  and `crosscodex admin cleaning decide` (which also needs the `reviewer`
  role) accept them. Prints `added`, or `already registered`. A deleted
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

feedback:
  # Review queue thresholds, fractions in [0, 1]; global only (see Mapping Feedback).
  min_agreement: 0.5     # below this a voted mapping needs escalation and lists as high disagreement
  low_confidence: 0.5    # below this confidence the queue lists a mapping as low confidence
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

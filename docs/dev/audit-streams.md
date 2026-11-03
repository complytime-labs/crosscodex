# Audit Streams

This document covers the NATS JetStream audit trail system, provenance headers, and how to inspect audit messages.

## Overview

CrossCodex uses [NATS JetStream](https://docs.nats.io/nats-concepts/jetstream) for persistent audit trails. Every message published through `pkg/natsbus` carries provenance headers that establish a chain of custody: who sent the message, when, what tenant it belongs to, a SHA-256 content hash, and the OpenTelemetry trace context that links the message to the distributed trace.

Three audit streams are created at startup. Messages without valid provenance headers are rejected on the subscriber side (fail-closed).

## Audit Streams

| Stream            | Subject Pattern                  | Retention  | Content                                |
|-------------------|----------------------------------|------------|----------------------------------------|
| `AUDIT_LLM`       | `crosscodex.audit.*.llm.>`       | 90 days    | LLM prompts, responses, model versions |
| `AUDIT_DECISIONS` | `crosscodex.audit.*.decisions.>` | Indefinite | Final compliance determinations        |
| `AUDIT_EVENTS`    | `crosscodex.audit.*.events.>`    | 30 days    | Pipeline lifecycle events, debugging   |

The `*` in each subject pattern is the tenant ID. All audit subjects are scoped per-tenant by design.

### LLM Call Events

`buildSharedResources` (`cmd/crosscodexd/resources.go`) attaches an `internal/worker.NATSAuditEmitter` to every LLM client it opens, so each completion and embedding call from the daemon roles and the admin one-shots publishes an `llmclient.AuditEvent` to `crosscodex.audit.<tenant>.llm.<job_id>`. The event records the tenant, job ID, model, operation, a SHA-256 hash of the prompt, prompt name and version, tokens used, duration, success or error, and the trace ID.

- **Fail closed at startup:** a caller that asks for the `llm` resource without `nats` is refused, so no LLM client is ever opened unaudited.
- **Best effort per call:** a publish failure is logged and counted in `crosscodex.worker.audit.failures.total` (by `reason`), but does not fail the LLM call (the `llmclient.AuditEmitter` contract).
- **Job ID:** the subject token, so it must be non-empty, free of `.`, `*` and `>`, and at most 128 bytes (`natsbus.MaxJobIDLength`); the emitter drops events that fail this check and counts them as `invalid_subject`. Worker calls (dispatched by the pipeline's analyzers) use the analysis job ID. `crosscodexd admin adjudicate artifacts` uses one ID per tenant run, `artifact-adjudication-<uuid>`, logged as `run.id` in the run's summary line; its events also carry the `artifact_same_as` prompt name and version stored in each verdict's evidence.
- **Admin one-shots attach to the daemon's NATS server:** see [Stream Creation](#stream-creation). Admin one-shots do not call `telemetry.Init` (see [telemetry.md](telemetry.md)), so their events carry an empty `trace_id` and the failure counter is not exported.

### Retention Configuration

LLM and events stream retention is configurable:

```yaml
nats:
  streams:
    audit_llm_retention: 2160h      # 90 days (default)
    audit_events_retention: 720h    # 30 days (default)
```

The decisions stream has no retention limit. Compliance determinations are kept indefinitely because auditors may need to review them years later.

### Stream Creation

Streams are created or updated automatically when the NATS client starts (`natsbus.New`). If a stream already exists, its configuration is updated to match. Manual stream management is not required.

Admin one-shots that publish audit events (`crosscodexd admin retention scan`, and `admin adjudicate artifacts` without `--dry-run`) instead attach with `natsbus.WithExistingStreams` (`requiredResources.attachNATS` in `cmd/crosscodexd/resources.go`). That mode:

- refuses an empty `nats.url` (`ErrExternalURLRequired`) before starting anything, since an embedded server would share the daemon's JetStream store directory;
- never creates or updates a stream, so a one-shot run with a different `nats.streams.*` cannot change the daemon's retention;
- fails with `ErrStreamNotFound` when an audit stream is missing; start the daemon, which creates them.

## Provenance Headers

Every message published through `pkg/natsbus` carries five mandatory provenance headers. These are injected automatically by `injectProvenance` on the publish path and validated by `extractProvenance` on the subscribe path.

| Header             | Value                             | Source                           |
|--------------------|-----------------------------------|----------------------------------|
| `X-Trace-Id`       | 32-character hex OTel trace ID    | Active span in `context.Context` |
| `X-Span-Id`        | 16-character hex OTel span ID     | Active span in `context.Context` |
| `X-Tenant-Id`      | Tenant identifier                 | `tenant.FromContext(ctx)`        |
| `X-Timestamp`      | RFC3339Nano UTC timestamp         | `time.Now().UTC()` at publish    |
| `X-Content-SHA256` | SHA-256 hex digest of the payload | `sha256.Sum256(data)`            |

> **Note**: `msg.Metadata.Timestamp` on the subscriber side is the subscriber's local `time.Now()`, not the published value. Access the original publish time via `msg.Headers["X-Timestamp"]`.

### Content Hash Verification

The content hash provides tamper detection. When a subscriber receives a message, `wrapHandler` automatically recomputes `sha256.Sum256(msg.Data)` and compares it against the `X-Content-SHA256` header before the application handler runs. A mismatch causes the message to be rejected with a logged error; the application handler never sees it.

The hash is computed deterministically: identical payloads always produce identical hashes. This is tested explicitly in the BDD test suite.

### Provenance on the Publish Path

When you call `Publish` or `PublishWithHeaders`, the client:

1. Extracts the tenant ID from the context (fails if missing).
2. Builds provenance headers from the active span and payload.
3. Merges user-provided headers with provenance headers (provenance wins on conflict).
4. Publishes the message with all headers attached.

```go
// Provenance is automatic — no extra code needed.
err := client.Publish(ctx, "crosscodex.audit.acme-corp.events.pipeline.started", payload)
```

If you need custom headers alongside provenance:

```go
err := client.PublishWithHeaders(ctx, subject, payload, map[string][]string{
    "X-Custom-Header": {"value"},
})
```

### Provenance on the Subscribe Path

When a message arrives, `wrapHandler` runs before the application handler:

1. Extracts provenance headers. If any mandatory header is missing or empty, the message is rejected with an error log listing the missing fields. The application handler never sees the message.
2. Reconstructs a `trace.SpanContext` from the trace and span IDs and marks it as remote.
3. Injects the remote span context into the handler's `context.Context` so any spans created during processing appear as children of the publisher's span.

The error message for missing provenance lists the specific fields that are absent:

```
msg="rejecting message: missing provenance" subject=crosscodex.audit.acme-corp.events.test error="missing provenance headers: X-Tenant-Id, X-Content-SHA256"
```

### Exception: the Worker Progress Signal

`crosscodex.workers.progress` (`natsbus.WorkerProgressSubject`) is the one subject that carries no tenant ID and no provenance. LLM workers publish a tick on it after every task result they publish, and each analysis collection listens to it so its idle timeout (`analysis.engine.task_timeout`) measures the whole worker pool, not just its own results. Workers are shared by every tenant, job and daemon on the server, so a tick that named its tenant would show every tenant's collections which tenants are active, and the trace headers would link their traces.

The exception is narrow:

- Only `Client.PublishWorkerProgress` and `Client.SubscribeWorkerProgress` skip the tenant and provenance rules, and only for this subject. A tick has an empty payload and no headers; it must never carry a tenant ID, trace context, task identity or any other data.
- The tick handler is a `func()`: it receives nothing from the message, so nothing on the wire reaches application logic, even when a provenanced message is published to the subject.
- `Publish`, `PublishWithHeaders`, `Subscribe` and `QueueSubscribe` treat the subject like any other: publishing without a tenant fails, and a generic subscriber drops ticks for missing provenance. That drop is logged at debug level, not error, so a catch-all subscriber such as `crosscodex.>` is not flooded at the pool's task rate.
- A tick, forged or legitimate, can only keep a collection waiting longer before its idle timeout (`analysis.engine.task_timeout`) fires, and at most until the stage's hard cap, `pipeline.stage_timeout` (default 24h), which ticks never reset. It cannot deliver, alter or suppress a result.

### Control Messages: Abandoned Collections

```
crosscodex.control.{tenant_id}.{task_type}.{job_id}.{collection_id}.abandoned
```

An analysis collection (one analyzer stage of one job waiting for its LLM task results) that ends without every expected result publishes one message on this subject (`natsbus.AbandonedSubject`). It ends that way on the idle timeout (`analysis.engine.task_timeout`), the stage cap (`pipeline.stage_timeout`), a cancelled job context (which is how `CancelJob` reaches a running stage), or a failed dispatch. Without it, the tasks the collection already dispatched would still run on the worker pool, be billed, and have their results discarded.

- **Tenant-scoped, with provenance.** It is published with `Publish` on the collection's tenant, so it carries the normal provenance headers, and its `X-Tenant-Id` matches the tenant token of the subject.
- **No payload.** The subject is the whole key; the message carries no task content.
- **Keyed by collection, not by job.** `collection_id` is a random UUID created for each collection and sent with every task it dispatches or redispatches, in the `X-Collection-Id` work header. A resumed or retried job reuses its job ID but gets new collections, so an abandon from an earlier run never matches a later run's tasks.
- **Best effort.** A failed publish is logged and counted (`analysis.collections.abandoned{outcome="publish_failed"}`, see [telemetry.md](telemetry.md)) and never changes the stage's error. Its only cost is the wasted LLM calls the message exists to prevent.

The collection also stops its own retries still waiting on their backoff.

Every LLM worker subscribes to `crosscodex.control.*.*.*.*.abandoned` (`natsbus.AbandonedSubjectFilter`) with a plain subscription, not the `llm-workers` queue group, so each worker learns of each abandon; the subscription is made before the work subscription, and `Start` refuses a worker without an abandon TTL (`worker.ErrNoAbandonTTL`).

- **Validation.** The handler is a deserialization point for messages anyone on the bus can publish. It drops, logs at warn and counts (`worker.errors{error_category="invalid_control_message"}`) a message whose subject does not parse, whose provenance tenant differs from the subject's tenant, or which carries a payload. `natsbus.ParseAbandonedSubject` accepts only what `natsbus.AbandonedSubject` builds: a valid tenant ID, one of the `natsbus.TaskType` constants, a job ID without NATS delimiters of at most 128 bytes, and a canonical lower-case UUID collection ID, so each collection has exactly one subject. A dropped message changes nothing, and the handler never returns an error. A task with missing `X-Task-Id` or `X-Task-Type` headers is counted as `invalid_message` before the abandon check, never as dropped.
- **Dropping tasks.** Before decoding a task's payload, the worker checks its (tenant, task type, job, `X-Collection-Id`) against the set. A match, first dispatch or retry, is dropped: no LLM call, no result, no worker progress tick. It is counted in `worker.tasks.dropped{reason="abandoned"}` and logged at debug with its task, job and collection IDs, never its content. A task without `X-Collection-Id` is never dropped.
- **Bounded memory.** Each worker remembers an abandon for its own host's `pipeline.stage_timeout`, the longest any collection can live, and at most `worker.max_abandoned_collections` abandons (both per worker host; in a split deployment, worker hosts must use the same `pipeline.stage_timeout` as pipeline hosts or a larger one, otherwise workers forget abandons while collections still run and execute tasks nobody reads) (default 10000, range [1, 100000], global-only), evicting the oldest first. An entry costs about 430 bytes with the UUID job IDs the pipeline generates, so about 4.3 MB at the default, and at most about 510 bytes (5.1 MB at the default, 51 MB at the maximum), because every subject builder and `natsbus.ParseAbandonedSubject` reject a job ID longer than `natsbus.MaxJobIDLength` (128 bytes) with `natsbus.ErrInvalidJobID`. Expired entries are removed whenever a task or abandon is checked, and `worker.abandoned_collections` reports the count each time it changes. Raise the cap when more collections are abandoned within one `stage_timeout` than it holds: each evicted abandon lets that collection's queued tasks run and be billed. Because the key includes the collection ID, a forgotten abandon only lets that collection's remaining tasks run as before, and a remembered one can never drop another collection's tasks.

- **Forgery.** The provenance tenant check is a consistency check that rejects mismatched messages, not authorization: provenance headers are unsigned, so any bus participant can set `X-Tenant-Id` to match the subject it publishes on, for any tenant. What protects a collection is that its ID is a random UUID seen only in work message headers, and NATS publish permissions on `crosscodex.control.>` (tenant isolation on NATS is subject-level only; per-tenant accounts are not implemented). A forged abandon makes the workers drop that collection's tasks, so the stage fails on its idle timeout or stage cap; it cannot alter or forge a result.

Tasks already calling the LLM when the abandon arrives finish normally; their results are discarded by the collector, which no longer listens.

## Trace Context Propagation

NATS messages carry trace context through the `X-Trace-Id` and `X-Span-Id` provenance headers. On the subscriber side, `reconstructSpanContext` parses these into a `trace.SpanContext` with `Remote: true` and `TraceFlags: Sampled`. This context is set on the handler's `context.Context` via `trace.ContextWithRemoteSpanContext`.

This means a trace that starts in one service, publishes to NATS, and is processed by another service appears as a single connected trace in Jaeger. The subscriber's processing spans are children of the publisher's span.

**Current limitation**: The current implementation uses custom headers (`X-Trace-Id`, `X-Span-Id`) rather than the W3C `traceparent` format. Interoperability with external systems that expect W3C trace context headers requires translation at the boundary.

## Inspecting Audit Messages

### Using the NATS CLI

If you have the `nats` CLI installed, you can inspect messages in the audit streams:

1. **List all streams**:
   ```bash
   nats stream list
   ```

2. **View stream info**:
   ```bash
   nats stream info AUDIT_DECISIONS
   ```

3. **Read messages from a stream**:
   ```bash
   nats stream view AUDIT_DECISIONS
   ```

4. **Subscribe to live events** (for a specific tenant):
   ```bash
   nats sub "crosscodex.audit.acme-corp.events.>"
   ```

5. **Get a specific message by sequence number**:
   ```bash
   nats stream get AUDIT_DECISIONS 42
   ```

### Reading Provenance from Messages

When inspecting a raw NATS message, the provenance headers are in the message headers section:

```
NATS-Message Subject: crosscodex.audit.acme-corp.decisions.mapping.complete
X-Trace-Id: a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4
X-Span-Id: 1a2b3c4d5e6f7a8b
X-Tenant-Id: acme-corp
X-Timestamp: 2026-06-03T15:30:00.123456789Z
X-Content-SHA256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
```

To correlate with the distributed trace, copy the `X-Trace-Id` value and search for it in Jaeger (see [Telemetry](telemetry.md) for Jaeger setup).

### Correlating Audit Messages with Traces

1. Find the audit message in the NATS stream (via CLI or through the application).
2. Extract the `X-Trace-Id` header value.
3. Open the Jaeger UI at `http://localhost:16686`.
4. Paste the trace ID into the search bar.
5. The trace shows the full processing chain, including the publish operation, any intermediate processing, and the subscriber's handling.

This bidirectional link means an auditor can start from either the audit message or the trace and reach the other.

## Embedded vs External Mode

`pkg/natsbus` supports two modes:

- **Embedded**: When `nats.url` is empty, an embedded NATS server starts in-process. JetStream data is stored under `$XDG_STATE_HOME/crosscodex/nats/` (configurable via `nats.embedded.store_dir`). Suitable for single-node deployments and development.

- **External**: When `nats.url` is set (e.g., `tls://nats:4222`), the client connects to an external NATS server or cluster. Required for distributed deployments.

Both modes support TLS. The embedded server uses the global TLS configuration from `pkg/tlsconfig`. The external mode uses `nats.tls: true` or a custom TLS config passed via options.

Audit streams behave identically in both modes.

## Security Considerations

- Tenant isolation is enforced at the subject level. Every NATS subject includes the tenant ID (e.g., `crosscodex.audit.acme-corp.events.>`). Services must extract the tenant ID from the authenticated context, not from user input.
- Messages without provenance headers are rejected on the subscriber side with an error-level log entry listing the missing fields. The application handler never processes unauthenticated messages. The worker progress tick is the only exception, and its handler receives no message data (see [Exception: the Worker Progress Signal](#exception-the-worker-progress-signal)).
- Content hashes provide integrity verification but not encryption. Payload confidentiality depends on TLS transport encryption.
- The `AUDIT_DECISIONS` stream has indefinite retention. Plan storage capacity accordingly for long-running deployments.

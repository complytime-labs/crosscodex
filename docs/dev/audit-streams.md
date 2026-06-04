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

## Provenance Headers

Every message published through `pkg/natsbus` carries five mandatory provenance headers. These are injected automatically by `injectProvenance` on the publish path and validated by `extractProvenance` on the subscribe path.

| Header             | Value                             | Source                             |
|--------------------|-----------------------------------|------------------------------------|
| `X-Trace-Id`       | 32-character hex OTel trace ID    | Active span in `context.Context`   |
| `X-Span-Id`        | 16-character hex OTel span ID     | Active span in `context.Context`   |
| `X-Tenant-Id`      | Tenant identifier                 | `tenant.FromContext(ctx)`          |
| `X-Timestamp`      | RFC3339Nano UTC timestamp         | `time.Now().UTC()` at publish time |
| `X-Content-SHA256` | SHA-256 hex digest of the payload | `sha256.Sum256(data)`              |

### Content Hash Verification

The content hash provides tamper detection. When a subscriber receives a message, it can recompute `sha256.Sum256(msg.Data)` and compare it against `msg.Metadata.ContentHash`. A mismatch indicates the payload was corrupted in transit.

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

## Trace Context Propagation

NATS messages carry trace context through the `X-Trace-Id` and `X-Span-Id` provenance headers. On the subscriber side, `reconstructSpanContext` parses these into a `trace.SpanContext` with `Remote: true` and `TraceFlags: Sampled`. This context is set on the handler's `context.Context` via `trace.ContextWithRemoteSpanContext`.

This means a trace that starts in one service, publishes to NATS, and is processed by another service appears as a single connected trace in Jaeger. The subscriber's processing spans are children of the publisher's span.

**Current limitation**: The current implementation uses custom headers (`X-Trace-Id`, `X-Span-Id`) rather than the W3C `traceparent` format. Interoperability with external systems that expect W3C trace context headers requires translation at the boundary.

## Inspecting Audit Messages

### Using the NATS CLI

If you have the `nats` CLI installed, you can inspect messages in the audit streams:

```bash
# List all streams
nats stream list

# View stream info
nats stream info AUDIT_DECISIONS

# Read messages from a stream
nats stream view AUDIT_DECISIONS

# Subscribe to live events (for a specific tenant)
nats sub "crosscodex.audit.acme-corp.events.>"

# Get a specific message by sequence number
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
- Messages without provenance headers are rejected on the subscriber side with an error-level log entry listing the missing fields. The application handler never processes unauthenticated messages.
- Content hashes provide integrity verification but not encryption. Payload confidentiality depends on TLS transport encryption.
- The `AUDIT_DECISIONS` stream has indefinite retention. Plan storage capacity accordingly for long-running deployments.

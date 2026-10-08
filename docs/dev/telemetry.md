# Telemetry

This document covers configuring, reading, and extending OpenTelemetry instrumentation in CrossCodex.

## Overview

CrossCodex uses [OpenTelemetry](https://opentelemetry.io/) for distributed tracing, metrics, and structured log correlation. The `pkg/telemetry` package provides initialization, instrument factories, and correlation helpers. All signals export via OTLP (OpenTelemetry Protocol, gRPC or HTTP) to a collector such as Jaeger, Grafana Tempo, or the OpenTelemetry Collector.

An empty endpoint disables the signal entirely (no-op provider, no error). This means a local development setup with no collector configured runs without telemetry overhead.

## Configuration

Add an `observability` section to your CrossCodex config file:

```yaml
observability:
  endpoint: "localhost:4317"     # Shared OTLP endpoint for all signals
  protocol: grpc                 # grpc | http

  tracing:
    endpoint: ""                 # Per-signal override; empty = use shared endpoint
    protocol: ""                 # Per-signal override; empty = use shared protocol
    sample_rate: 1.0             # 0.0 to 1.0; defaults to 1.0 when endpoint is set

  metrics:
    endpoint: ""                 # Per-signal override
    protocol: ""                 # Per-signal override
    interval: "30s"              # Collection interval (Go duration format)
```

The resolution logic mirrors `pkg/tlsconfig`: per-signal fields override the shared default when non-empty. This lets you send traces to one backend and metrics to another if needed.

### Validation Rules

- `sample_rate` must be between 0.0 and 1.0 (inclusive).
- `protocol` must be `grpc` or `http`.
- `interval` must parse as a Go `time.Duration` (e.g., `30s`, `1m`, `500ms`).
- An invalid config causes `telemetry.Init` to return an error. The service will not start with a misconfigured telemetry pipeline.

## Initialization

Services call `telemetry.Init` at startup. This registers a global `TracerProvider`, `MeterProvider` and W3C TraceContext + Baggage propagator, wraps the default `slog` handler to inject trace IDs, and returns a shutdown function. `crosscodexd` calls it (service name `crosscodexd`) before building any resource, refuses to start if it fails, and runs the shutdown last so spans emitted while stopping are flushed. The `crosscodex` CLI does the same (service name `crosscodex`) before starting its embedded gateway, and flushes when the embedded daemon stops or fails to start:

```go
shutdown, err := telemetry.Init(ctx, cfg.Observability,
    telemetry.WithServiceName("crosscodex-catalog"),
    telemetry.WithServiceVersion(version.Version),
)
if err != nil {
    log.Fatalf("telemetry init: %v", err)
}
defer shutdown(ctx)
```

After `Init` returns, `otel.GetTracerProvider()` and `otel.GetMeterProvider()` return the configured providers. Package-level tracers created via `otel.GetTracerProvider().Tracer("crosscodex/pkg/mypackage")` produce real spans when an endpoint is configured and no-op spans otherwise.

### Service Instrumentation

Most packages only emit their own spans and metrics when their telemetry option is passed at construction. `crosscodexd`'s bootstrap (`cmd/crosscodexd/bootstrap.go`, `resources.go`) and the CLI's embedded mode (`cmd/crosscodex/embedded.go`) pass one to every service they build:

- Options that take providers (`WithTelemetry(tp, mp)` in `internal/*`, `pkg/analyzer`, `pkg/llmclient`) get `otel.GetTracerProvider()` and `otel.GetMeterProvider()`. Each package names its own tracer `crosscodex/<pkg path>` and uses the `crosscodex` meter.
- Options that take a `(trace.Tracer, metric.Meter)` pair (gateway, catalog, authn, attestation, db, natsbus, vectordb, graphdb, storage, the OSCAL parser) get `telemetry.Instrumentation("<pkg path>")`, which follows the same naming.
- `pipeline.NewProductionRegistry` and `pipeline.BuildCandidateRegistry` take providers and forward them to the analyzers, candidate providers and builtin candidate generators they construct.

A span's instrumentation scope therefore tells you which package produced it. Paths that are not instrumented, and why:

| Path | Reason |
|------|--------|
| `db.NewMigrator`, `db.NewTenantPool` | No telemetry option; they trace through the global provider directly |
| `storage.NewFromConfig`, `storage.NewArchiveFromConfig` (retention engine) | No option parameter |
| `prompt.NewRegistry` | Accepts providers but never uses them |
| `pipeline.NewCandidateGenerator` and its PG readers | No instrumentation |
| `pkg/analyzer/consensus` inside the requires analyzer | `Compute` takes no context, so its spans would be disconnected roots |
| `crosscodexd admin retention scan` | One-shot command that does not call `telemetry.Init` |
| `crosscodexd admin reconcile artifacts` | One-shot command that does not call `telemetry.Init` |
| `crosscodexd admin backup *` | One-shot command that does not call `telemetry.Init` |

## Traces

### How Spans Are Created

Each instrumented package creates spans on public method calls:

```go
var tracer = otel.GetTracerProvider().Tracer("crosscodex/pkg/mypackage")

func (s *Service) DoWork(ctx context.Context) error {
    ctx, span := tracer.Start(ctx, "mypackage.DoWork")
    defer span.End()

    span.SetAttributes(
        attribute.String("tenant.id", tenantID),
        attribute.String("operation.type", "classify"),
    )

    if err != nil {
        span.RecordError(err)
        span.SetStatus(codes.Error, err.Error())
        return err
    }
    span.SetStatus(codes.Ok, "")
    return nil
}
```

### Reading Traces in Jaeger

1. Start the Jaeger container from the integration test compose file:

   ```bash
   podman-compose -f test/compose.yaml --profile telemetry up -d jaeger
   ```

   This starts Jaeger all-in-one with OTLP gRPC on port 14317, OTLP HTTP on port 14318, and the query UI on port 16686.

2. Configure CrossCodex to export to Jaeger:

   ```yaml
   observability:
     endpoint: "localhost:14317"
     protocol: grpc
   ```

3. Open the Jaeger UI at `http://localhost:16686`.

4. Select the service name (e.g., `crosscodex-catalog`) from the service dropdown.

5. Each trace shows the full call chain. Key attributes to look for:

   | Attribute           | Meaning                                     |
   |---------------------|---------------------------------------------|
   | `tenant.id`         | Which tenant the operation belongs to       |
   | `messaging.subject` | NATS subject (for natsbus operations)       |
   | `storage.key`       | Object storage key (for storage operations) |
   | `db.operation`      | Database operation type                     |
   | `auth.method`       | Authentication method used (for authn)      |
   | `auth.success`      | Whether authentication succeeded            |

### Cross-Service Trace Propagation

Connect RPCs carry trace context in the W3C `traceparent` header. The gateway and pipeline servers install the `otelconnect` interceptor, which reads the global propagator registered by `telemetry.Init`.

NATS messages carry trace context in provenance headers instead. When a service publishes a message, `pkg/natsbus` injects the current trace and span IDs as `X-Trace-Id` and `X-Span-Id` headers. The subscriber reconstructs a remote `SpanContext` from these headers, so new spans created during message processing appear as children of the publishing span.

See [Audit Streams](audit-streams.md) for details on the provenance header system.

## Metrics

### Instrument Factories

`pkg/telemetry` provides thin wrappers that enforce the `crosscodex` meter namespace:

```go
counter, _ := telemetry.NewCounter("mypackage.operations.total")
histogram, _ := telemetry.NewHistogram("mypackage.duration_ms")
gauge, _ := telemetry.NewGauge("mypackage.pool.utilization")
intCounter, _ := telemetry.NewIntCounter("mypackage.errors.total")
```

All instruments are created under the `crosscodex` meter name, so they group together in metric backends.

For components whose telemetry option takes a `(trace.Tracer, metric.Meter)` pair rather than a provider pair (e.g. `gateway.WithTelemetry`, `db.WithTelemetry`), `telemetry.Instrumentation(component)` returns both: a tracer named `crosscodex/<component>` and the shared `crosscodex` meter, matching the naming convention provider-style packages use for their own tracer.

### Registered Metrics

| Metric                              | Type             | Package            | Description                               |
|-------------------------------------|------------------|--------------------|-------------------------------------------|
| `natsbus.publish.total`             | Int64Counter     | pkg/natsbus        | Total messages published                  |
| `natsbus.publish.duration_ms`       | Int64Histogram   | pkg/natsbus        | Publish latency in ms                     |
| `natsbus.process.total`             | Int64Counter     | pkg/natsbus        | Messages processed by subscriber handlers |
| `natsbus.process.duration_ms`       | Int64Histogram   | pkg/natsbus        | Subscriber handler execution duration     |
| `db.queries.total`                  | Int64Counter     | pkg/db             | Total queries executed                    |
| `db.query.duration_ms`              | Int64Histogram   | pkg/db             | Query latency in ms                       |
| `db.transactions.total`             | Int64Counter     | pkg/db             | Total transactions started                |
| `db.pool.open_connections`          | Int64Gauge       | pkg/db             | Current open connections                  |
| `authn.attempts.total`              | Int64Counter     | pkg/authn          | Authentication attempts                   |
| `authn.duration_ms`                 | Int64Histogram   | pkg/authn          | Authentication latency                    |
| `graphdb.queries.total`             | Int64Counter     | pkg/graphdb        | Graph driver calls by operation, status (ok or error) and result (ok, exists, not_found or error) |
| `graphdb.query.duration_ms`         | Int64Histogram   | pkg/graphdb        | Graph driver call duration in milliseconds by operation, status and result |
| `graph.materialize.edges_skipped.total` | Int64Counter | internal/graph     | Edges the NATS subscriber skipped because an endpoint node was missing, by `analyzer` and `edge.label`. Nonzero means the graph lacks edges until it is rebuilt; the warning log names the tenant, edge and endpoint IDs, and the `edge skipped: endpoint node missing` span event (on the `graph.materialize.<analyzer>` span, which carries `tenant.id`) names the edge and endpoint IDs |
| `storage.operations.total`          | Int64Counter     | pkg/storage        | Storage operations                        |
| `storage.operation.duration_ms`     | Int64Histogram   | pkg/storage        | Storage operation latency                 |
| `vectordb.searches.total`           | Int64Counter     | pkg/vectordb       | Vector similarity searches                |
| `vectordb.search.duration_ms`       | Int64Histogram   | pkg/vectordb       | Search latency                            |
| `vectordb.embeddings.stored.total`  | Int64Counter     | pkg/vectordb       | Embeddings stored                         |
| `vectordb.store.duration_ms`        | Int64Histogram   | pkg/vectordb       | Store latency                             |
| `llmclient.completions.total`       | Int64Counter     | pkg/llmclient      | Completion requests                       |
| `llmclient.completion.duration_ms`  | Int64Histogram   | pkg/llmclient      | Completion request duration               |
| `llmclient.embeddings.total`        | Int64Counter     | pkg/llmclient      | Embedding requests                        |
| `llmclient.embedding.duration_ms`   | Int64Histogram   | pkg/llmclient      | Embedding request duration                |
| `llmclient.errors.total`            | Int64Counter     | pkg/llmclient      | LLM client errors                         |
| `synthesis.executions.total`        | Int64Counter     | internal/synthesis | Synthesis executions                      |
| `synthesis.errors.total`            | Int64Counter     | internal/synthesis | Synthesis errors by category              |
| `synthesis.duration_ms`             | Float64Histogram | internal/synthesis | Synthesis execution duration              |
| `synthesis.pairs.ranked.total`      | Int64Counter     | internal/synthesis | Pairs ranked                              |
| `synthesis.viability.updates.total` | Int64Counter     | internal/synthesis | Viability database updates                |
| `backup.operations.total`           | Int64Counter     | pkg/backup         | Backup commands by `operation` (`run`, `list`, `verify`, `restore`) and `result` (`ok`, `error`) |
| `backup.step.duration_ms`           | Float64Histogram | pkg/backup         | Duration of each store's capture during `backup run`, by `store` (`postgres`, `objects`, `nats`) |
| `backup.bytes.total`                | Int64Counter     | pkg/backup         | Bytes captured per store during `backup run`, by `store`; recorded only on that step's success |
| `backup.verify.failures.total`      | Int64Counter     | pkg/backup         | Integrity problems found by `backup verify` (manifest issues plus WAL `FAILURE` checks), summed per call; not recorded when zero |
| `backup.staleness_seconds`          | Float64Gauge     | pkg/backup         | Age of each store's newest backup point in seconds, by `store`; absent (not zero) when no complete point exists |

`crosscodexd admin backup *` does not call `telemetry.Init` (see the uninstrumented-paths table above), so these instruments exist but are never exported today.

### Graph Driver Metric Attributes

`pkg/graphdb/agedriver` records `graphdb.queries.total` and `graphdb.query.duration_ms` once per call, with the same three attributes on both:

| Attribute   | Values |
|-------------|--------|
| `operation` | `create_graph`, `create_node`, `upsert_node`, `create_edge`, `create_requires_edge`, `bulk_create_edges`, `query_relationships`, `query_as_of`, `traverse`, `get_node`, `get_edge`, `execute_query`, `supersede_fact` |
| `status`    | `ok`, `error` |
| `result`    | `ok`, `exists`, `not_found`, `error` |

Counting rules:

- A call that returns no error records `status=ok`, `result=ok`.
- Domain sentinels are outcomes, not faults, and record `status=ok`: `ErrNodeExists` and `ErrEdgeExists` give `result=exists`; `ErrNodeNotFound` and `ErrEdgeNotFound` give `result=not_found`. Idempotent materializer re-runs meet "already exists" for every rebuilt fact, so counting them as errors would turn rebuild volume into error-rate alerts.
- A write rejected because an endpoint node is missing (`CreateEdge`, `BulkCreateEdges`, `CreateRequiresEdge`) also records `status=ok`, `result=not_found`: it is a caller-input outcome, not a driver fault. Dashboards for write failures should filter on `result!=ok`, not only `status=error`.
- Every other error, including a tenant or query rejected after the span starts, records `status=error`, `result=error`.
- Spans follow the same classification. A domain sentinel marks the span `Ok` and sets the span attribute `graphdb.result` to `exists` or `not_found`, because OpenTelemetry discards the description of an `Ok` status. Every other error marks the span `Error` with the error message, except tenant errors, whose fixed status is set when the tenant is rejected so the rejected value never reaches the trace.
- Argument checks that run before the span starts (for example a node without an ID) record no metrics and no span.
- A call that panics is recorded as `status=ok`; the driver does not recover panics to count them.
- Tenant IDs are never metric attributes; the span carries `tenant.id`.

## Logs

### Automatic Trace Correlation

After `telemetry.Init` runs, the default `slog` handler is wrapped with a `traceHandler` that automatically injects `trace_id` and `span_id` attributes into every log record when a span with a valid trace or span ID is present in the context. This includes remote span contexts. No code changes are needed in logging call sites.

A log line with trace correlation looks like:

```json
{
  "time": "2026-06-03T15:30:00Z",
  "level": "INFO",
  "msg": "migration applied",
  "trace_id": "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4",
  "span_id": "1a2b3c4d5e6f7a8b"
}
```

To correlate a log entry with its trace in Jaeger, copy the `trace_id` value and search for it in the Jaeger UI.

`Init` wraps whatever handler `slog.Default()` has when it runs, so each binary installs its own handler first: `crosscodexd` builds one from `logging.level` and `logging.format`, and `crosscodex` builds one from its verbosity flags and `logging.level`. Wrapping slog's built-in handler is not an option: that handler writes through the `log` package, and `slog.SetDefault` points `log`'s output back at the wrapper, so the first log call deadlocks on `log`'s mutex.

### Correlation Helpers

For code that needs the trace or span ID as a string value (for example, to populate audit metadata or attestation records):

```go
traceID := telemetry.TraceIDFromContext(ctx)  // hex string or ""
spanID := telemetry.SpanIDFromContext(ctx)    // hex string or ""
```

These return empty strings when no valid trace context is present.

## Testing

### In-Memory Test Provider

`pkg/telemetry/telemetrytest` provides an in-memory provider that captures spans and metrics without network I/O:

```go
import "github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"

tp, err := telemetrytest.NewTestProvider()
Expect(err).NotTo(HaveOccurred())
// Use tp.TracerProvider() and tp.MeterProvider() in your test setup

// After exercising the code under test:
spans := tp.GetSpans()    // captured spans
metrics := tp.GetMetrics() // captured metrics
tp.Reset()                 // clear between test cases

defer func() { _ = tp.Shutdown(context.Background()) }()
```

### Integration Tests

The Jaeger container in `test/compose.yaml` (profile: `telemetry`) receives real OTLP data during integration tests. To run the telemetry integration tests:

```bash
task test:integration:telemetry
```

This starts the Jaeger container on port 14317, runs the integration test suite, and validates that spans and metrics arrive at the collector.

## Instrumentation Status

| Package            | Traces  | Metrics | Status                                                                                                                     |
|--------------------|---------|---------|----------------------------------------------------------------------------------------------------------------------------|
| pkg/vectordb       | Full    | Full    | Reference implementation                                                                                                   |
| pkg/storage        | Full    | Full    | Complete                                                                                                                   |
| pkg/graphdb        | Full    | Full    | Complete                                                                                                                   |
| pkg/authn          | Full    | Full    | Complete                                                                                                                   |
| pkg/db             | Partial | Full    | Transaction wrapper (`pgTx`) has no spans                                                                                  |
| pkg/natsbus        | Full    | Full    | Per-message consumer spans (natsbus.process), subscriber metrics (process counter, duration histogram)                     |
| pkg/llmclient      | Full    | Full    | Spans on Complete/Embed; 5 instruments (completions, embeddings, errors counters; completion/embedding latency histograms) |
| pkg/oscal          | Partial | None    | `oscal.Parse` / `oscal.Structure` spans with `WithParserTracer` / `WithStructurerTracer`; binaries build only the parser  |
| internal/synthesis | Full    | Full    | Span on Service.Execute; 5 instruments (executions/errors/pairs/updates counters; duration histogram)                      |

When implementing new packages or extending existing ones, follow `pkg/vectordb` as the reference for span structure, attribute naming, and metric registration.

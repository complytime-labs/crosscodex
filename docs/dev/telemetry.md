# Telemetry

This document covers configuring, reading, and extending OpenTelemetry instrumentation in CrossCodex.

## Overview

CrossCodex uses [OpenTelemetry](https://opentelemetry.io/) for distributed tracing, metrics, and structured log correlation. The `pkg/telemetry` package provides initialization, instrument factories, and correlation helpers. All signals export via OTLP (gRPC or HTTP) to a collector such as Jaeger, Grafana Tempo, or the OpenTelemetry Collector.

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

Services call `telemetry.Init` at startup. This registers a global `TracerProvider` and `MeterProvider`, wraps the default `slog` handler to inject trace IDs, and returns a shutdown function:

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

Trace context flows across service boundaries through NATS provenance headers. When a service publishes a message, `pkg/natsbus` injects the current trace and span IDs as `X-Trace-Id` and `X-Span-Id` headers. The subscriber reconstructs a remote `SpanContext` from these headers, so new spans created during message processing appear as children of the publishing span.

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

### Registered Metrics

| Metric                             | Type           | Package      | Description                |
|------------------------------------|----------------|--------------|----------------------------|
| `natsbus.publish.total`            | Int64Counter   | pkg/natsbus  | Total messages published   |
| `natsbus.publish.duration_ms`      | Int64Histogram | pkg/natsbus  | Publish latency in ms      |
| `db.queries.total`                 | Int64Counter   | pkg/db       | Total queries executed     |
| `db.query.duration_ms`             | Int64Histogram | pkg/db       | Query latency in ms        |
| `db.transactions.total`            | Int64Counter   | pkg/db       | Total transactions started |
| `db.pool.open_connections`         | Int64Gauge     | pkg/db       | Current open connections   |
| `authn.attempts.total`             | Int64Counter   | pkg/authn    | Authentication attempts    |
| `authn.duration_ms`                | Int64Histogram | pkg/authn    | Authentication latency     |
| `graphdb.queries.total`            | Int64Counter   | pkg/graphdb  | Graph queries executed     |
| `graphdb.query.duration_ms`        | Int64Histogram | pkg/graphdb  | Graph query latency        |
| `storage.operations.total`         | Int64Counter   | pkg/storage  | Storage operations         |
| `storage.operation.duration_ms`    | Int64Histogram | pkg/storage  | Storage operation latency  |
| `vectordb.searches.total`          | Int64Counter   | pkg/vectordb | Vector similarity searches |
| `vectordb.search.duration_ms`      | Int64Histogram | pkg/vectordb | Search latency             |
| `vectordb.embeddings.stored.total` | Int64Counter   | pkg/vectordb | Embeddings stored          |
| `vectordb.store.duration_ms`       | Int64Histogram | pkg/vectordb | Store latency              |

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

| Package       | Traces  | Metrics      | Status                                                                                       |
|---------------|---------|--------------|----------------------------------------------------------------------------------------------|
| pkg/vectordb  | Full    | Full         | Reference implementation                                                                     |
| pkg/storage   | Full    | Full         | Complete                                                                                     |
| pkg/graphdb   | Full    | Full         | Complete                                                                                     |
| pkg/authn     | Full    | Full         | Complete                                                                                     |
| pkg/db        | Partial | Full         | Transaction wrapper (`pgTx`) has no spans                                                    |
| pkg/natsbus   | Partial | Publish only | Subscribe/QueueSubscribe setup has spans; no per-message handler spans or subscriber metrics |
| pkg/llmclient | None    | None         | Scaffold only                                                                                |
| pkg/oscal     | None    | None         | Scaffold only                                                                                |

When implementing new packages or extending existing ones, follow `pkg/vectordb` as the reference for span structure, attribute naming, and metric registration.

package graphdb

import (
	"fmt"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Option configures a GraphDB client.
type Option func(*ageClient) error

// WithTelemetry configures OpenTelemetry tracing and metrics for the graph client.
func WithTelemetry(tracer trace.Tracer, meter metric.Meter) Option {
	return func(c *ageClient) error {
		c.tracer = tracer
		c.meter = meter

		var err error
		c.queryCounter, err = meter.Int64Counter("graphdb.queries.total",
			metric.WithDescription("Total graph queries executed"))
		if err != nil {
			return fmt.Errorf("create query counter: %w", err)
		}
		c.queryLatency, err = meter.Int64Histogram("graphdb.query.duration_ms",
			metric.WithDescription("Graph query duration in milliseconds"))
		if err != nil {
			return fmt.Errorf("create query latency histogram: %w", err)
		}
		return nil
	}
}

package graph

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// PGResolver resolves resources from PostgreSQL.
type PGResolver struct {
	conn   db.TenantConnection
	tracer trace.Tracer

	resolveCounter metric.Int64Counter
	resolveLatency metric.Float64Histogram
	resolveBytes   metric.Int64Counter
}

// PGResolverOption configures a PGResolver.
type PGResolverOption func(*PGResolver)

// WithPGResolverTelemetry enables OTel tracing and metrics on the PG resolver.
func WithPGResolverTelemetry(tp trace.TracerProvider, mp metric.MeterProvider) PGResolverOption {
	return func(r *PGResolver) {
		if tp != nil {
			r.tracer = tp.Tracer("crosscodex/internal/graph")
		}
		if mp != nil {
			m := mp.Meter("crosscodex")
			r.resolveCounter, _ = m.Int64Counter("graph.resolve.total")
			r.resolveLatency, _ = m.Float64Histogram("graph.resolve.duration_ms")
			r.resolveBytes, _ = m.Int64Counter("graph.resolve.bytes")
		}
	}
}

// NewPGResolver creates a PostgreSQL resource resolver.
func NewPGResolver(conn db.TenantConnection, opts ...PGResolverOption) *PGResolver {
	r := &PGResolver{conn: conn}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Scheme returns "pg".
func (r *PGResolver) Scheme() string { return "pg" }

// Resolve fetches analysis results from PostgreSQL by parsing the URI path.
// URI format: pg://results/{job_id}/{analyzer_name}
func (r *PGResolver) Resolve(ctx context.Context, ref ResourceRef) ([]byte, error) {
	start := time.Now()
	ctx, span := telemetry.StartSpan(r.tracer, ctx, "graph.resolve")
	defer span.End()
	span.SetAttributes(
		attribute.String("ref.type", ref.Type),
		attribute.String("ref.id", ref.ID),
		attribute.String("ref.scheme", "pg"),
	)

	// Parse the URI path to extract job_id and analyzer.
	// URI format: pg://results/{job_id}/{analyzer_name}
	jobID, analyzerName, err := parsePGURI(ref.URI)
	if err != nil {
		r.recordResolve(ctx, start, 0, "error")
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("pg resolve: %w", err)
	}

	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		r.recordResolve(ctx, start, 0, "error")
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("pg resolve job %s analyzer %s: %w", jobID, analyzerName, err)
	}

	tx, err := r.conn.Begin(ctx)
	if err != nil {
		r.recordResolve(ctx, start, 0, "error")
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("pg resolve job %s analyzer %s: %w", jobID, analyzerName, err)
	}
	defer func() { _ = tx.Rollback() }()

	query := `SELECT result_data FROM analysis_results WHERE job_id = $1 AND analyzer_name = $2 AND tenant_id = $3`
	var data []byte
	if err := tx.QueryRow(ctx, query, jobID, analyzerName, tenantID).Scan(&data); err != nil {
		r.recordResolve(ctx, start, 0, "error")
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("pg resolve job %s analyzer %s: %w", jobID, analyzerName, err)
	}

	if err := tx.Commit(); err != nil {
		r.recordResolve(ctx, start, 0, "error")
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("pg resolve job %s analyzer %s: committing: %w", jobID, analyzerName, err)
	}

	r.recordResolve(ctx, start, int64(len(data)), "ok")
	span.SetStatus(codes.Ok, "")
	return data, nil
}

func (r *PGResolver) recordResolve(ctx context.Context, start time.Time, bytes int64, status string) {
	attrs := metric.WithAttributes(
		attribute.String("scheme", "pg"),
		attribute.String("status", status),
	)
	if r.resolveCounter != nil {
		r.resolveCounter.Add(ctx, 1, attrs)
	}
	if r.resolveLatency != nil {
		r.resolveLatency.Record(ctx, float64(time.Since(start).Milliseconds()),
			metric.WithAttributes(attribute.String("scheme", "pg")))
	}
	if r.resolveBytes != nil && bytes > 0 {
		r.resolveBytes.Add(ctx, bytes,
			metric.WithAttributes(attribute.String("scheme", "pg")))
	}
}

// parsePGURI extracts job_id and analyzer_name from "pg://results/{job_id}/{analyzer}".
func parsePGURI(uri string) (jobID, analyzer string, err error) {
	const prefix = "pg://results/"
	if len(uri) <= len(prefix) {
		return "", "", fmt.Errorf("invalid pg URI: %s", uri)
	}
	path := uri[len(prefix):]
	parts := splitFirst(path, '/')
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid pg URI path: expected {job_id}/{analyzer}, got %s", path)
	}
	return parts[0], parts[1], nil
}

// splitFirst splits s at the first occurrence of sep, returning [before, after].
// Returns [s] if sep is not found.
func splitFirst(s string, sep byte) []string {
	for i := range len(s) {
		if s[i] == sep {
			return []string{s[:i], s[i+1:]}
		}
	}
	return []string{s}
}

var _ ResourceResolver = (*PGResolver)(nil)

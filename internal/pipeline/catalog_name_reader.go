package pipeline

import (
	"context"
	"database/sql"
	"errors"
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

// CatalogNameReader reads an imported catalog's name: the uploader's
// catalog_name, or the OSCAL metadata title when none was given. The pair
// analyzers show it as each control's framework.
type CatalogNameReader interface {
	CatalogName(ctx context.Context, tenantID, catalogID string) (string, error)
}

type pgCatalogNameReader struct {
	db     db.TenantConnection
	tracer trace.Tracer

	readCounter metric.Int64Counter
	readLatency metric.Float64Histogram
}

// PGCatalogNameReaderOption configures the reader NewPGCatalogNameReader
// builds.
type PGCatalogNameReaderOption func(*pgCatalogNameReader)

// WithPGCatalogNameReaderTelemetry enables OTel tracing and metrics on the
// catalog name reader. crosscodexd passes the global providers, as it does
// for the pipeline store.
func WithPGCatalogNameReaderTelemetry(tp trace.TracerProvider, mp metric.MeterProvider) PGCatalogNameReaderOption {
	return func(r *pgCatalogNameReader) {
		if tp != nil {
			r.tracer = tp.Tracer("crosscodex/internal/pipeline")
		}
		if mp != nil {
			m := mp.Meter("crosscodex")
			r.readCounter, _ = m.Int64Counter("pipeline.catalogs.name.total",
				metric.WithDescription("Catalog name reads, by status"))
			r.readLatency, _ = m.Float64Histogram("pipeline.catalogs.name.duration_ms",
				metric.WithDescription("Catalog name read duration"))
		}
	}
}

// NewPGCatalogNameReader constructs a CatalogNameReader backed by Postgres.
func NewPGCatalogNameReader(conn db.TenantConnection, opts ...PGCatalogNameReaderOption) CatalogNameReader {
	r := &pgCatalogNameReader{db: conn}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// CatalogName reads catalogID's name for tenantID. It returns
// ErrCatalogNotFound when the tenant has no such catalog: RLS and the
// tenant_id predicate both hide other tenants' catalogs.
func (r *pgCatalogNameReader) CatalogName(ctx context.Context, tenantID, catalogID string) (string, error) {
	start := time.Now()
	ctx, span := telemetry.StartSpan(r.tracer, ctx, "pipeline.catalogs.name")
	defer span.End()
	span.SetAttributes(
		attribute.String("operation", "catalog_name"),
		attribute.String("catalog.id", catalogID),
	)

	name, err := r.catalogName(ctx, span, tenantID, catalogID)
	status := "ok"
	if err != nil {
		status = "error"
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	} else {
		span.SetStatus(codes.Ok, "")
	}
	if r.readCounter != nil {
		r.readCounter.Add(ctx, 1, metric.WithAttributes(
			attribute.String("operation", "catalog_name"), attribute.String("status", status)))
	}
	if r.readLatency != nil {
		r.readLatency.Record(ctx, float64(time.Since(start).Milliseconds()))
	}
	return name, err
}

// catalogName is CatalogName without its telemetry.
func (r *pgCatalogNameReader) catalogName(ctx context.Context, span trace.Span, tenantID, catalogID string) (string, error) {
	if err := tenant.ValidateTenantID(tenantID); err != nil {
		return "", fmt.Errorf("pgCatalogNameReader.CatalogName: %w", err)
	}
	span.SetAttributes(attribute.String("tenant.id", tenantID))

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("pgCatalogNameReader.CatalogName: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var name string
	err = tx.QueryRow(ctx, `SELECT name FROM catalogs WHERE tenant_id = $1 AND catalog_id = $2`, tenantID, catalogID).Scan(&name)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("pgCatalogNameReader.CatalogName: catalog %s: %w", catalogID, ErrCatalogNotFound)
	case err != nil:
		return "", fmt.Errorf("pgCatalogNameReader.CatalogName: reading catalog %s: %w", catalogID, err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("pgCatalogNameReader.CatalogName: commit failed: %w", err)
	}
	return name, nil
}

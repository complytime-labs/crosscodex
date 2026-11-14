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

// CatalogControl is the projection of one controls row that the graph role
// turns into a Control node.
type CatalogControl struct {
	ControlID  string // node ID, "<catalog_id>/<identifier>"
	CatalogID  string
	Identifier string
	Title      string
	Statement  string
	Class      string
	ParentID   string // raw parent identifier of a control or part within the catalog; empty for a top-level control
	NodeKind   string // props "node-kind" (control or statement-part); empty when the row has none
	Label      string // props "label": the source's published label, e.g. "AC-2(7)"; empty when the row has none
	SortID     string // props "sort-id": the source's published sort key; empty when the row has none

	// ContainerKey names the catalog_containers row of the group this item sits
	// in; empty for an ungrouped item. Every item of a grouped control carries
	// it, but only a top-level control hangs off its container in the graph.
	ContainerKey string
}

// CatalogContainer is the projection of one catalog_containers row that the
// graph role turns into a Container node.
type CatalogContainer struct {
	CatalogID  string
	Key        string // position key within the catalog, e.g. "g2.1"
	Identifier string // the group's source ID; empty when the source gave none
	Title      string
	Label      string // the group's published label; empty when the source gave none
	ParentKey  string // key of the enclosing container; empty for a top-level one
}

// ControlsReader lists a catalog's controls and containers for the tenant in
// ctx. Relational rows are authoritative; the graph is projected from them.
type ControlsReader interface {
	ListCatalogControls(ctx context.Context, catalogID string) ([]CatalogControl, error)
	ListCatalogContainers(ctx context.Context, catalogID string) ([]CatalogContainer, error)
}

// PGControlsReader reads controls through a tenant connection, whose
// transactions scope RLS to the tenant in ctx; the query also binds that
// tenant explicitly, as the catalog store does.
type PGControlsReader struct {
	conn   db.TenantConnection
	tracer trace.Tracer

	listCounter metric.Int64Counter
	listLatency metric.Float64Histogram

	containerCounter metric.Int64Counter
	containerLatency metric.Float64Histogram
}

// PGControlsReaderOption configures a PGControlsReader.
type PGControlsReaderOption func(*PGControlsReader)

// WithPGControlsReaderTelemetry enables OTel tracing and metrics on the
// controls reader.
func WithPGControlsReaderTelemetry(tp trace.TracerProvider, mp metric.MeterProvider) PGControlsReaderOption {
	return func(r *PGControlsReader) {
		if tp != nil {
			r.tracer = tp.Tracer("crosscodex/internal/graph")
		}
		if mp != nil {
			m := mp.Meter("crosscodex")
			r.listCounter, _ = m.Int64Counter("graph.controls.list.total")
			r.listLatency, _ = m.Float64Histogram("graph.controls.list.duration_ms")
			r.containerCounter, _ = m.Int64Counter("graph.containers.list.total")
			r.containerLatency, _ = m.Float64Histogram("graph.containers.list.duration_ms")
		}
	}
}

// NewPGControlsReader creates a ControlsReader over conn.
func NewPGControlsReader(conn db.TenantConnection, opts ...PGControlsReaderOption) *PGControlsReader {
	r := &PGControlsReader{conn: conn}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// ListCatalogControls returns every control of catalogID, ordered by control_id.
func (r *PGControlsReader) ListCatalogControls(ctx context.Context, catalogID string) ([]CatalogControl, error) {
	start := time.Now()
	ctx, span := telemetry.StartSpan(r.tracer, ctx, "graph.controls.list")
	defer span.End()
	span.SetAttributes(attribute.String("catalog.id", catalogID))

	out, err := r.listCatalogControls(ctx, span, catalogID)
	if err != nil {
		r.recordList(ctx, start, "error")
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetAttributes(attribute.Int("controls.count", len(out)))
	r.recordList(ctx, start, "ok")
	span.SetStatus(codes.Ok, "")
	return out, nil
}

func (r *PGControlsReader) listCatalogControls(ctx context.Context, span trace.Span, catalogID string) ([]CatalogControl, error) {
	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("list controls of catalog %s: %w", catalogID, err)
	}
	span.SetAttributes(attribute.String("tenant.id", tenantID))

	// A tenant connection sets app.current_tenant only inside a transaction.
	tx, err := r.conn.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("list controls of catalog %s: %w", catalogID, err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.Query(ctx, `
		SELECT control_id, catalog_id, identifier, COALESCE(title, ''), COALESCE(statement, ''),
		       COALESCE(class, ''), COALESCE(parent_id, ''), COALESCE(props->>'node-kind', ''), COALESCE(props->>'label', ''), COALESCE(props->>'sort-id', ''), COALESCE(container_key, '')
		FROM controls WHERE catalog_id = $1 AND tenant_id = $2 ORDER BY control_id`, catalogID, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list controls of catalog %s: %w", catalogID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []CatalogControl
	for rows.Next() {
		var c CatalogControl
		if err := rows.Scan(&c.ControlID, &c.CatalogID, &c.Identifier, &c.Title, &c.Statement, &c.Class, &c.ParentID, &c.NodeKind, &c.Label, &c.SortID, &c.ContainerKey); err != nil {
			return nil, fmt.Errorf("scan control of catalog %s: %w", catalogID, err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate controls of catalog %s: %w", catalogID, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit listing controls of catalog %s: %w", catalogID, err)
	}
	return out, nil
}

// ListCatalogContainers returns every container of catalogID, ordered by
// container_key.
func (r *PGControlsReader) ListCatalogContainers(ctx context.Context, catalogID string) ([]CatalogContainer, error) {
	start := time.Now()
	ctx, span := telemetry.StartSpan(r.tracer, ctx, "graph.containers.list")
	defer span.End()
	span.SetAttributes(attribute.String("catalog.id", catalogID))

	out, err := r.listCatalogContainers(ctx, span, catalogID)
	if err != nil {
		recordListing(ctx, r.containerCounter, r.containerLatency, start, "error")
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetAttributes(attribute.Int("containers.count", len(out)))
	recordListing(ctx, r.containerCounter, r.containerLatency, start, "ok")
	span.SetStatus(codes.Ok, "")
	return out, nil
}

func (r *PGControlsReader) listCatalogContainers(ctx context.Context, span trace.Span, catalogID string) ([]CatalogContainer, error) {
	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("list containers of catalog %s: %w", catalogID, err)
	}
	span.SetAttributes(attribute.String("tenant.id", tenantID))

	// A tenant connection sets app.current_tenant only inside a transaction.
	tx, err := r.conn.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("list containers of catalog %s: %w", catalogID, err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.Query(ctx, `
		SELECT catalog_id, container_key, identifier, title, label, COALESCE(parent_key, '')
		FROM catalog_containers WHERE catalog_id = $1 AND tenant_id = $2 ORDER BY container_key`, catalogID, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list containers of catalog %s: %w", catalogID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []CatalogContainer
	for rows.Next() {
		var c CatalogContainer
		if err := rows.Scan(&c.CatalogID, &c.Key, &c.Identifier, &c.Title, &c.Label, &c.ParentKey); err != nil {
			return nil, fmt.Errorf("scan container of catalog %s: %w", catalogID, err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate containers of catalog %s: %w", catalogID, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit listing containers of catalog %s: %w", catalogID, err)
	}
	return out, nil
}

func (r *PGControlsReader) recordList(ctx context.Context, start time.Time, status string) {
	recordListing(ctx, r.listCounter, r.listLatency, start, status)
}

func recordListing(ctx context.Context, counter metric.Int64Counter, latency metric.Float64Histogram, start time.Time, status string) {
	if counter != nil {
		counter.Add(ctx, 1, metric.WithAttributes(attribute.String("status", status)))
	}
	if latency != nil {
		latency.Record(ctx, float64(time.Since(start).Milliseconds()))
	}
}

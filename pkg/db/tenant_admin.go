package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// Tenant lifecycle states. Migration 007's tenants_status_check allows
// exactly these; the gateway refuses every tenant that is not active.
const (
	TenantStatusActive    = "active"
	TenantStatusSuspended = "suspended"
)

// ErrTenantNotFound reports an operation on a tenant that has no tenants row.
var ErrTenantNotFound = errors.New("tenant not found")

// TenantRecord is one tenants row as list_tenants() returns it.
type TenantRecord struct {
	ID          string
	DisplayName string
	Status      string
	CreatedAt   time.Time
}

// TenantSpec is a tenant to create, or to rename if it exists.
type TenantSpec struct {
	ID          string
	DisplayName string
}

// TenantAdmin provisions and suspends tenants as the tenant_admin role
// (migration 007). That role holds no table privilege: it can only call the
// SECURITY DEFINER functions provision_tenant, set_tenant_status and
// list_tenants, so it cannot read tenant data or bulk-modify tenants. This
// is the only Go code that calls those functions.
type TenantAdmin struct {
	pool Pool
	tel  *tenantAdminInstruments
}

// OpenTenantAdmin connects to dsn, which must authenticate as tenant_admin.
// opts are pool options; WithTelemetry also instruments the TenantAdmin
// methods, which otherwise use telemetry.Instrumentation("pkg/db").
func OpenTenantAdmin(dsn string, opts ...Option) (*TenantAdmin, error) {
	// opts are applied here only to read the tracer and meter; NewPool
	// applies them again, which is safe because options are pure setters.
	o := defaultOptions()
	for _, opt := range opts {
		opt(&o)
	}
	// One connection: commands are sequential, and Import's transaction
	// holds it for its whole batch.
	pool, err := NewPool(PoolConfig{DSN: dsn, MaxOpenConns: 1}, opts...)
	if err != nil {
		return nil, fmt.Errorf("connect as tenant_admin: %w", err)
	}
	a, err := newTenantAdmin(pool, o.tracer, o.meter)
	if err != nil {
		return nil, errors.Join(err, pool.Close())
	}
	return a, nil
}

func newTenantAdmin(pool Pool, tracer trace.Tracer, meter metric.Meter) (*TenantAdmin, error) {
	if tracer == nil || meter == nil {
		defaultTracer, defaultMeter := telemetry.Instrumentation("pkg/db")
		if tracer == nil {
			tracer = defaultTracer
		}
		if meter == nil {
			meter = defaultMeter
		}
	}
	tel, err := newTenantAdminInstruments(tracer, meter)
	if err != nil {
		return nil, err
	}
	return &TenantAdmin{pool: pool, tel: tel}, nil
}

// Close closes the connection.
func (a *TenantAdmin) Close() error { return a.pool.Close() }

// Provision creates the tenant, and through the tenant_graph_create trigger
// its graph, or renames it if it exists, leaving its status unchanged.
// created reports whether it inserted.
func (a *TenantAdmin) Provision(ctx context.Context, id, displayName string) (created bool, err error) {
	ctx, done := a.tel.op(ctx, "provision")
	defer func() { done(err) }()
	if err := ValidateTenantSpec(TenantSpec{ID: id, DisplayName: displayName}); err != nil {
		return false, err
	}
	setTenantAttribute(ctx, id)
	return provisionTenant(ctx, a.pool, id, displayName)
}

// SetStatus sets the tenant's status to TenantStatusActive or
// TenantStatusSuspended and returns the previous status. An unknown tenant
// returns an error wrapping ErrTenantNotFound.
func (a *TenantAdmin) SetStatus(ctx context.Context, id, status string) (previous string, err error) {
	ctx, done := a.tel.op(ctx, "set_status")
	defer func() { done(err) }()
	if err := tenant.ValidateTenantID(id); err != nil {
		return "", err
	}
	setTenantAttribute(ctx, id)
	if status != TenantStatusActive && status != TenantStatusSuspended {
		return "", fmt.Errorf("tenant status %q is invalid: it must be %q or %q", status, TenantStatusActive, TenantStatusSuspended)
	}
	err = a.pool.QueryRow(ctx, "SELECT public.set_tenant_status($1, $2)", id, status).Scan(&previous)
	if IsPgErrorCode(err, "P0002") { // no_data_found, raised by set_tenant_status
		return "", fmt.Errorf("set tenant %q status: %w: %w", id, ErrTenantNotFound, err)
	}
	if err != nil {
		return "", fmt.Errorf("set tenant %q status to %q: %w", id, status, err)
	}
	return previous, nil
}

// List returns every tenant ordered by ID.
func (a *TenantAdmin) List(ctx context.Context) (records []TenantRecord, err error) {
	ctx, done := a.tel.op(ctx, "list")
	defer func() { done(err) }()
	rows, err := a.pool.Query(ctx, "SELECT tenant_id, display_name, status, created_at FROM public.list_tenants()")
	if err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	defer func() {
		err = errors.Join(err, rows.Close())
		if err != nil {
			records = nil
		}
	}()
	for rows.Next() {
		var r TenantRecord
		if err := rows.Scan(&r.ID, &r.DisplayName, &r.Status, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("list tenants: read row: %w", err)
		}
		records = append(records, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	return records, nil
}

// Get returns one tenant. An unknown tenant returns an error wrapping
// ErrTenantNotFound.
func (a *TenantAdmin) Get(ctx context.Context, id string) (record TenantRecord, err error) {
	ctx, done := a.tel.op(ctx, "get")
	defer func() { done(err) }()
	if err := tenant.ValidateTenantID(id); err != nil {
		return TenantRecord{}, err
	}
	setTenantAttribute(ctx, id)
	err = a.pool.QueryRow(ctx,
		"SELECT tenant_id, display_name, status, created_at FROM public.list_tenants() WHERE tenant_id = $1", id).
		Scan(&record.ID, &record.DisplayName, &record.Status, &record.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return TenantRecord{}, fmt.Errorf("tenant %q does not exist. Create it with 'crosscodexd admin tenant create --tenant %s --display-name <name>': %w",
			id, id, ErrTenantNotFound)
	}
	if err != nil {
		return TenantRecord{}, fmt.Errorf("get tenant %q: %w", id, err)
	}
	return record, nil
}

// Import provisions every spec in one transaction: either all are created
// or renamed, or none are. created[i] reports whether specs[i] was inserted.
func (a *TenantAdmin) Import(ctx context.Context, specs []TenantSpec) (created []bool, err error) {
	ctx, done := a.tel.op(ctx, "import")
	defer func() { done(err) }()
	if err := ValidateTenantSpecs(specs); err != nil {
		return nil, err
	}
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("import tenants: %w", err)
	}
	created = make([]bool, len(specs))
	for i, s := range specs {
		c, err := provisionTenant(ctx, tx, s.ID, s.DisplayName)
		if err != nil {
			return nil, errors.Join(
				fmt.Errorf("import tenants: tenant %d of %d: %w. Nothing was imported; fix this entry and run the import again", i+1, len(specs), err),
				tx.Rollback())
		}
		created[i] = c
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("import tenants: commit: %w", err)
	}
	return created, nil
}

// rowQuerier is the QueryRow method Pool and Transaction share.
type rowQuerier interface {
	QueryRow(ctx context.Context, query string, args ...any) Row
}

func provisionTenant(ctx context.Context, q rowQuerier, id, displayName string) (bool, error) {
	var created bool
	if err := q.QueryRow(ctx, "SELECT public.provision_tenant($1, $2)", id, displayName).Scan(&created); err != nil {
		return false, fmt.Errorf("provision tenant %q: %w", id, err)
	}
	return created, nil
}

// ValidateTenantSpec applies provision_tenant's rules in Go: a valid
// tenant ID and a display name that is neither blank nor holds a control
// character (which would corrupt tabular output and terminals).
func ValidateTenantSpec(s TenantSpec) error {
	if err := tenant.ValidateTenantID(s.ID); err != nil {
		return err
	}
	if strings.TrimSpace(s.DisplayName) == "" {
		return fmt.Errorf("tenant %q: display name is empty; give the tenant a human-readable name", s.ID)
	}
	if strings.ContainsFunc(s.DisplayName, unicode.IsControl) {
		return fmt.Errorf("tenant %q: display name contains a control character; use printable text only", s.ID)
	}
	return nil
}

// ValidateTenantSpecs checks an Import batch: at least one spec, each
// valid, and no tenant ID twice, which is a bug in the batch rather than
// an upsert. Positions in errors are 1-based.
func ValidateTenantSpecs(specs []TenantSpec) error {
	if len(specs) == 0 {
		return errors.New("no tenants to import: the list is empty. Add at least one tenant")
	}
	firstAt := make(map[string]int, len(specs))
	for i, s := range specs {
		if err := ValidateTenantSpec(s); err != nil {
			return fmt.Errorf("tenant %d: %w", i+1, err)
		}
		if j, dup := firstAt[s.ID]; dup {
			return fmt.Errorf("tenants %d and %d both have ID %q; each tenant may appear only once. Remove or merge one of them", j, i+1, s.ID)
		}
		firstAt[s.ID] = i + 1
	}
	return nil
}

// TenantActive reports whether tenantID has a tenants row with status
// active. It calls tenant_is_active(), which app_user may execute: the
// tenants RLS policy hides every row outside a tenant transaction. An
// unknown tenant is not active.
func TenantActive(ctx context.Context, conn Connection, tenantID string) (bool, error) {
	if err := tenant.ValidateTenantID(tenantID); err != nil {
		return false, err
	}
	var active bool
	if err := conn.QueryRow(ctx, "SELECT public.tenant_is_active($1)", tenantID).Scan(&active); err != nil {
		return false, fmt.Errorf("check tenant %q status: %w", tenantID, err)
	}
	return active, nil
}

// tenantAdminInstruments holds TenantAdmin's spans and metrics
// (docs/dev/telemetry.md).
type tenantAdminInstruments struct {
	tracer     trace.Tracer
	operations metric.Int64Counter     // db.tenant_admin.operations.total{operation,result}
	duration   metric.Float64Histogram // db.tenant_admin.duration_ms{operation}
}

func newTenantAdminInstruments(tracer trace.Tracer, meter metric.Meter) (*tenantAdminInstruments, error) {
	in := &tenantAdminInstruments{tracer: tracer}
	var err error
	if in.operations, err = meter.Int64Counter("db.tenant_admin.operations.total",
		metric.WithDescription("Tenant administration calls by operation and result")); err != nil {
		return nil, fmt.Errorf("create db.tenant_admin.operations.total: %w", err)
	}
	if in.duration, err = meter.Float64Histogram("db.tenant_admin.duration_ms",
		metric.WithDescription("Duration of each tenant administration call in milliseconds")); err != nil {
		return nil, fmt.Errorf("create db.tenant_admin.duration_ms: %w", err)
	}
	return in, nil
}

// setTenantAttribute tags the call's span with the tenant. Callers do so
// only after validating id, so raw caller input never reaches the trace
// backend.
func setTenantAttribute(ctx context.Context, id string) {
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("tenant.id", id))
}

// op starts a span for one call; the returned func records its result and
// duration and ends the span. The span carries only the operation name;
// see setTenantAttribute for the tenant.
func (in *tenantAdminInstruments) op(ctx context.Context, operation string) (context.Context, func(error)) {
	start := time.Now()
	ctx, span := in.tracer.Start(ctx, "db.TenantAdmin."+operation,
		trace.WithAttributes(attribute.String("operation", operation)))
	return ctx, func(err error) {
		result := "ok"
		if err != nil {
			result = "error"
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		opAttr := attribute.String("operation", operation)
		in.operations.Add(ctx, 1, metric.WithAttributes(opAttr, attribute.String("result", result)))
		in.duration.Record(ctx, float64(time.Since(start))/float64(time.Millisecond), metric.WithAttributes(opAttr))
		span.End()
	}
}

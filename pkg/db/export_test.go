package db

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// ExportedPoolOptions exposes poolOptions fields for external test packages.
type ExportedPoolOptions struct {
	MaxIdleConns int
	ConnMaxLife  time.Duration
}

// ExportDefaultOptions calls defaultOptions and returns the values
// in an exported struct for testing from package db_test.
func ExportDefaultOptions() ExportedPoolOptions {
	o := defaultOptions()
	return ExportedPoolOptions{
		MaxIdleConns: o.maxIdleConns,
		ConnMaxLife:  o.connMaxLife,
	}
}

// ExportApplyOption applies an Option to default poolOptions and returns
// the result for inspection in external tests.
func ExportApplyOption(opt Option) ExportedPoolOptions {
	o := defaultOptions()
	opt(&o)
	return ExportedPoolOptions{
		MaxIdleConns: o.maxIdleConns,
		ConnMaxLife:  o.connMaxLife,
	}
}

// ExportRedactDSN exposes redactDSN for direct testing from package db_test.
func ExportRedactDSN(dsn string) string {
	return redactDSN(dsn)
}

// ExportNewClosedPool creates a pgPool with closed=true for testing
// Close() idempotency from package db_test.
func ExportNewClosedPool() Connection {
	return &pgPool{closed: true}
}

// ExportNewErrRow creates an errRow for testing Scan() behavior
// from package db_test.
func ExportNewErrRow(err error) Row {
	return &errRow{err: err}
}

// TelemetryFields exposes telemetry instrument state for test assertions.
type TelemetryFields struct {
	HasTracer       bool
	HasMeter        bool
	HasQueryCounter bool
	HasQueryLatency bool
	HasTxCounter    bool
	HasConnGauge    bool
}

// ExportTelemetryFields returns telemetry instrument state from a pgPool.
func ExportTelemetryFields(p Pool) TelemetryFields {
	pp, ok := p.(*pgPool)
	if !ok {
		return TelemetryFields{}
	}
	return TelemetryFields{
		HasTracer:       pp.tracer != nil,
		HasMeter:        pp.meter != nil,
		HasQueryCounter: pp.queryCounter != nil,
		HasQueryLatency: pp.queryLatency != nil,
		HasTxCounter:    pp.txCounter != nil,
		HasConnGauge:    pp.connGauge != nil,
	}
}

// ErrExportPoolUsed is what every call on ExportNewFailingPool's pool returns.
var ErrExportPoolUsed = errors.New("test pool was used")

// failingPool fails every call and counts the calls, so specs can prove
// that validation rejected input before any query.
type failingPool struct{ calls int }

func (p *failingPool) Begin(context.Context) (Transaction, error) {
	p.calls++
	return nil, ErrExportPoolUsed
}
func (p *failingPool) Query(context.Context, string, ...any) (Rows, error) {
	p.calls++
	return nil, ErrExportPoolUsed
}
func (p *failingPool) QueryRow(context.Context, string, ...any) Row {
	p.calls++
	return &errRow{err: ErrExportPoolUsed}
}
func (p *failingPool) Exec(context.Context, string, ...any) error {
	p.calls++
	return ErrExportPoolUsed
}
func (p *failingPool) Close() error { return nil }
func (p *failingPool) Health(context.Context) (*HealthStatus, error) {
	p.calls++
	return nil, ErrExportPoolUsed
}
func (p *failingPool) VerifyExtensions(context.Context) error {
	p.calls++
	return ErrExportPoolUsed
}

// ExportNewFailingPool returns a Pool whose every call fails with
// ErrExportPoolUsed, and a func reporting how many calls reached it.
func ExportNewFailingPool() (Pool, func() int) {
	p := &failingPool{}
	return p, func() int { return p.calls }
}

// ExportNewTenantAdmin builds a TenantAdmin on pool. nil tracer or meter
// falls back to telemetry.Instrumentation("pkg/db"), as in OpenTenantAdmin.
func ExportNewTenantAdmin(pool Pool, tracer trace.Tracer, meter metric.Meter) (*TenantAdmin, error) {
	return newTenantAdmin(pool, tracer, meter)
}

// scriptedRow is a Row whose Scan returns a fixed error.
type scriptedRow struct{ err error }

func (r scriptedRow) Scan(...any) error { return r.err }

// scriptedTx answers its i-th QueryRow with rowErrs[i] (nil past the end),
// returns commitErr from Commit and rollbackErr from Rollback, and counts
// Commit and Rollback calls.
type scriptedTx struct {
	rowErrs                     []error
	commitErr, rollbackErr      error
	queries, commits, rollbacks int
}

func (t *scriptedTx) Commit() error   { t.commits++; return t.commitErr }
func (t *scriptedTx) Rollback() error { t.rollbacks++; return t.rollbackErr }
func (t *scriptedTx) Query(context.Context, string, ...any) (Rows, error) {
	return nil, ErrExportPoolUsed
}
func (t *scriptedTx) QueryRow(context.Context, string, ...any) Row {
	var err error
	if t.queries < len(t.rowErrs) {
		err = t.rowErrs[t.queries]
	}
	t.queries++
	return scriptedRow{err: err}
}
func (t *scriptedTx) Exec(context.Context, string, ...any) error { return ErrExportPoolUsed }

// scriptedPool hands out tx from Begin and answers QueryRow with rowErr;
// every other call fails like failingPool.
type scriptedPool struct {
	failingPool
	tx     *scriptedTx
	rowErr error
}

func (p *scriptedPool) Begin(context.Context) (Transaction, error) { return p.tx, nil }
func (p *scriptedPool) QueryRow(context.Context, string, ...any) Row {
	return scriptedRow{err: p.rowErr}
}

// ExportNewScriptedTxPool returns a Pool whose Begin yields a transaction
// whose i-th QueryRow Scan returns rowErrs[i], whose Commit returns
// commitErr and whose Rollback returns rollbackErr, plus a func reporting
// the Commit and Rollback counts.
func ExportNewScriptedTxPool(rowErrs []error, commitErr, rollbackErr error) (Pool, func() (commits, rollbacks int)) {
	tx := &scriptedTx{rowErrs: rowErrs, commitErr: commitErr, rollbackErr: rollbackErr}
	return &scriptedPool{tx: tx}, func() (int, int) { return tx.commits, tx.rollbacks }
}

// ExportNewScriptedRowPool returns a Pool whose every QueryRow Scan returns err.
func ExportNewScriptedRowPool(err error) Pool {
	return &scriptedPool{rowErr: err}
}

// scriptedRows yields n rows, each Scan returning scanErr without touching
// dest, then reports err from Err and closeErr from Close, counting Close
// calls.
type scriptedRows struct {
	n                      int
	scanErr, err, closeErr error
	closes                 int
}

func (r *scriptedRows) Next() bool {
	if r.n == 0 {
		return false
	}
	r.n--
	return true
}
func (r *scriptedRows) Scan(...any) error { return r.scanErr }
func (r *scriptedRows) Err() error        { return r.err }
func (r *scriptedRows) Close() error      { r.closes++; return r.closeErr }

// scriptedRowsPool answers Query with rows; every other call fails like
// failingPool.
type scriptedRowsPool struct {
	failingPool
	rows *scriptedRows
}

func (p *scriptedRowsPool) Query(context.Context, string, ...any) (Rows, error) { return p.rows, nil }

// ExportNewScriptedRowsPool returns a Pool whose Query yields n rows whose
// Scan returns scanErr, whose Err returns err and whose Close returns
// closeErr, plus a func reporting how many times Close was called.
func ExportNewScriptedRowsPool(n int, scanErr, err, closeErr error) (Pool, func() int) {
	rows := &scriptedRows{n: n, scanErr: scanErr, err: err, closeErr: closeErr}
	return &scriptedRowsPool{rows: rows}, func() int { return rows.closes }
}

// ExportSQLStateError is an error carrying a SQLSTATE, like pgconn.PgError.
type ExportSQLStateError string

func (e ExportSQLStateError) Error() string    { return "sqlstate " + string(e) }
func (e ExportSQLStateError) SQLState() string { return string(e) }

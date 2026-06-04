package db

import "time"

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

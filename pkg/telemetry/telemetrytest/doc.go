// Package telemetrytest provides in-memory OpenTelemetry providers for test
// assertions. Use NewTestProvider to capture spans and metrics without
// network I/O.
//
// Example:
//
//	tp, err := telemetrytest.NewTestProvider()
//	// ... exercise code that creates spans ...
//	spans := tp.GetSpans()
//	// assert on span names, attributes, etc.
package telemetrytest

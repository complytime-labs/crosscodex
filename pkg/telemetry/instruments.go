package telemetry

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

const meterName = "crosscodex"

// NewCounter creates a Float64Counter using the global MeterProvider
// with the crosscodex meter namespace.
func NewCounter(name string, opts ...metric.Float64CounterOption) (metric.Float64Counter, error) {
	return otel.GetMeterProvider().Meter(meterName).Float64Counter(name, opts...)
}

// NewHistogram creates a Float64Histogram using the global MeterProvider
// with the crosscodex meter namespace.
func NewHistogram(name string, opts ...metric.Float64HistogramOption) (metric.Float64Histogram, error) {
	return otel.GetMeterProvider().Meter(meterName).Float64Histogram(name, opts...)
}

// NewGauge creates a Float64Gauge using the global MeterProvider
// with the crosscodex meter namespace.
func NewGauge(name string, opts ...metric.Float64GaugeOption) (metric.Float64Gauge, error) {
	return otel.GetMeterProvider().Meter(meterName).Float64Gauge(name, opts...)
}

// NewIntCounter creates an Int64Counter using the global MeterProvider
// with the crosscodex meter namespace.
func NewIntCounter(name string, opts ...metric.Int64CounterOption) (metric.Int64Counter, error) {
	return otel.GetMeterProvider().Meter(meterName).Int64Counter(name, opts...)
}

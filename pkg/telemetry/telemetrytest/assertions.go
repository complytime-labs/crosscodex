package telemetrytest

import (
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// FindSpan returns the first span matching the given name, or nil if not found.
func FindSpan(spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	for _, s := range spans {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

// FindSpans returns all spans matching the given name.
func FindSpans(spans []sdktrace.ReadOnlySpan, name string) []sdktrace.ReadOnlySpan {
	var result []sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() == name {
			result = append(result, s)
		}
	}
	return result
}

// SpanAttribute extracts a named attribute value from a span.
// Returns the value and true if found, or a zero value and false if not.
func SpanAttribute(span sdktrace.ReadOnlySpan, key string) (attribute.Value, bool) {
	for _, a := range span.Attributes() {
		if string(a.Key) == key {
			return a.Value, true
		}
	}
	return attribute.Value{}, false
}

// FindMetric returns the first metric matching the given name from
// ResourceMetrics, or nil if not found.
func FindMetric(rm metricdata.ResourceMetrics, name string) *metricdata.Metrics {
	for _, sm := range rm.ScopeMetrics {
		for i := range sm.Metrics {
			if sm.Metrics[i].Name == name {
				return &sm.Metrics[i]
			}
		}
	}
	return nil
}

// CounterValue returns the summed int64 value from a Sum metric.
// Returns an error if the metric data is not a Sum type.
func CounterValue(m *metricdata.Metrics) (int64, error) {
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		return 0, fmt.Errorf("metric %q data is %T, not Sum[int64]", m.Name, m.Data)
	}
	var total int64
	for _, dp := range sum.DataPoints {
		total += dp.Value
	}
	return total, nil
}

// HistogramCount returns the total count across all data points of a
// Histogram metric. Returns an error if the metric data is not a Histogram type.
func HistogramCount(m *metricdata.Metrics) (uint64, error) {
	hist, ok := m.Data.(metricdata.Histogram[int64])
	if !ok {
		return 0, fmt.Errorf("metric %q data is %T, not Histogram[int64]", m.Name, m.Data)
	}
	var total uint64
	for _, dp := range hist.DataPoints {
		total += dp.Count
	}
	return total, nil
}

// Float64HistogramCount returns the total count across all data points of a
// Float64Histogram metric. Returns an error if the metric data is not a
// Histogram[float64] type.
func Float64HistogramCount(m *metricdata.Metrics) (uint64, error) {
	hist, ok := m.Data.(metricdata.Histogram[float64])
	if !ok {
		return 0, fmt.Errorf("metric %q data is %T, not Histogram[float64]", m.Name, m.Data)
	}
	var total uint64
	for _, dp := range hist.DataPoints {
		total += dp.Count
	}
	return total, nil
}

// GaugeValue returns the last recorded int64 value from a Gauge metric.
// If multiple data points exist, returns the value from the last one.
// Returns an error if the metric data is not a Gauge type or has no data points.
func GaugeValue(m *metricdata.Metrics) (int64, error) {
	gauge, ok := m.Data.(metricdata.Gauge[int64])
	if !ok {
		return 0, fmt.Errorf("metric %q data is %T, not Gauge[int64]", m.Name, m.Data)
	}
	if len(gauge.DataPoints) == 0 {
		return 0, fmt.Errorf("metric %q has no data points", m.Name)
	}
	return gauge.DataPoints[len(gauge.DataPoints)-1].Value, nil
}

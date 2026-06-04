package telemetrytest_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
)

func TestFindSpan(t *testing.T) {
	tp, err := telemetrytest.NewTestProvider()
	if err != nil {
		t.Fatalf("NewTestProvider: %v", err)
	}
	defer func() { _ = tp.Shutdown(context.Background()) }()

	tracer := tp.TracerProvider().Tracer("test")
	ctx := context.Background()

	_, span := tracer.Start(ctx, "op.Alpha")
	span.End()
	_, span2 := tracer.Start(ctx, "op.Beta")
	span2.End()

	spans := tp.GetSpans()

	found := telemetrytest.FindSpan(spans, "op.Alpha")
	if found == nil {
		t.Fatal("expected to find op.Alpha span")
	}
	if found.Name() != "op.Alpha" {
		t.Errorf("name = %q, want op.Alpha", found.Name())
	}

	notFound := telemetrytest.FindSpan(spans, "op.Gamma")
	if notFound != nil {
		t.Error("expected nil for nonexistent span")
	}
}

func TestFindSpans(t *testing.T) {
	tp, err := telemetrytest.NewTestProvider()
	if err != nil {
		t.Fatalf("NewTestProvider: %v", err)
	}
	defer func() { _ = tp.Shutdown(context.Background()) }()

	tracer := tp.TracerProvider().Tracer("test")
	ctx := context.Background()

	for range 3 {
		_, s := tracer.Start(ctx, "op.Repeated")
		s.End()
	}
	_, s := tracer.Start(ctx, "op.Other")
	s.End()

	spans := tp.GetSpans()
	matches := telemetrytest.FindSpans(spans, "op.Repeated")
	if len(matches) != 3 {
		t.Errorf("found %d spans, want 3", len(matches))
	}
}

func TestSpanAttribute(t *testing.T) {
	tp, err := telemetrytest.NewTestProvider()
	if err != nil {
		t.Fatalf("NewTestProvider: %v", err)
	}
	defer func() { _ = tp.Shutdown(context.Background()) }()

	tracer := tp.TracerProvider().Tracer("test")
	ctx := context.Background()

	_, span := tracer.Start(ctx, "op.WithAttrs",
		trace.WithAttributes(attribute.String("tenant.id", "acme")))
	span.End()

	spans := tp.GetSpans()
	found := telemetrytest.FindSpan(spans, "op.WithAttrs")
	if found == nil {
		t.Fatal("span not found")
	}

	val, ok := telemetrytest.SpanAttribute(found, "tenant.id")
	if !ok {
		t.Fatal("attribute tenant.id not found")
	}
	if val.AsString() != "acme" {
		t.Errorf("tenant.id = %q, want acme", val.AsString())
	}

	_, ok = telemetrytest.SpanAttribute(found, "nonexistent")
	if ok {
		t.Error("expected nonexistent attribute to return false")
	}
}

func TestFindMetric_Counter(t *testing.T) {
	tp, err := telemetrytest.NewTestProvider()
	if err != nil {
		t.Fatalf("NewTestProvider: %v", err)
	}
	defer func() { _ = tp.Shutdown(context.Background()) }()

	meter := tp.MeterProvider().Meter("test")
	counter, err := meter.Int64Counter("test.ops.total")
	if err != nil {
		t.Fatalf("Int64Counter: %v", err)
	}
	counter.Add(context.Background(), 5)

	rm := tp.GetMetrics()
	m := telemetrytest.FindMetric(rm, "test.ops.total")
	if m == nil {
		t.Fatal("metric not found")
	}

	val, err := telemetrytest.CounterValue(m)
	if err != nil {
		t.Fatalf("CounterValue: %v", err)
	}
	if val != 5 {
		t.Errorf("counter = %d, want 5", val)
	}
}

func TestFindMetric_Histogram(t *testing.T) {
	tp, err := telemetrytest.NewTestProvider()
	if err != nil {
		t.Fatalf("NewTestProvider: %v", err)
	}
	defer func() { _ = tp.Shutdown(context.Background()) }()

	meter := tp.MeterProvider().Meter("test")
	hist, err := meter.Int64Histogram("test.duration_ms")
	if err != nil {
		t.Fatalf("Int64Histogram: %v", err)
	}
	hist.Record(context.Background(), 42)
	hist.Record(context.Background(), 100)

	rm := tp.GetMetrics()
	m := telemetrytest.FindMetric(rm, "test.duration_ms")
	if m == nil {
		t.Fatal("metric not found")
	}

	count, err := telemetrytest.HistogramCount(m)
	if err != nil {
		t.Fatalf("HistogramCount: %v", err)
	}
	if count != 2 {
		t.Errorf("histogram count = %d, want 2", count)
	}
}

func TestCounterValue_WrongType(t *testing.T) {
	tp, err := telemetrytest.NewTestProvider()
	if err != nil {
		t.Fatalf("NewTestProvider: %v", err)
	}
	defer func() { _ = tp.Shutdown(context.Background()) }()

	meter := tp.MeterProvider().Meter("test")
	hist, _ := meter.Int64Histogram("test.wrong_type")
	hist.Record(context.Background(), 1)

	rm := tp.GetMetrics()
	m := telemetrytest.FindMetric(rm, "test.wrong_type")
	if m == nil {
		t.Fatal("metric not found")
	}

	_, err = telemetrytest.CounterValue(m)
	if err == nil {
		t.Error("expected error for histogram passed to CounterValue")
	}
}

func TestGaugeValue(t *testing.T) {
	tp, err := telemetrytest.NewTestProvider()
	if err != nil {
		t.Fatalf("NewTestProvider: %v", err)
	}
	defer func() { _ = tp.Shutdown(context.Background()) }()

	meter := tp.MeterProvider().Meter("test")
	gauge, err := meter.Int64Gauge("test.gauge")
	if err != nil {
		t.Fatalf("Int64Gauge: %v", err)
	}
	gauge.Record(context.Background(), 7)

	rm := tp.GetMetrics()
	m := telemetrytest.FindMetric(rm, "test.gauge")
	if m == nil {
		t.Fatal("metric not found")
	}

	val, err := telemetrytest.GaugeValue(m)
	if err != nil {
		t.Fatalf("GaugeValue: %v", err)
	}
	if val != 7 {
		t.Errorf("gauge value = %d, want 7", val)
	}
}

func TestGaugeValue_WrongType(t *testing.T) {
	tp, err := telemetrytest.NewTestProvider()
	if err != nil {
		t.Fatalf("NewTestProvider: %v", err)
	}
	defer func() { _ = tp.Shutdown(context.Background()) }()

	meter := tp.MeterProvider().Meter("test")
	counter, _ := meter.Int64Counter("test.not_gauge")
	counter.Add(context.Background(), 1)

	rm := tp.GetMetrics()
	m := telemetrytest.FindMetric(rm, "test.not_gauge")
	if m == nil {
		t.Fatal("metric not found")
	}

	_, err = telemetrytest.GaugeValue(m)
	if err == nil {
		t.Error("expected error for counter passed to GaugeValue")
	}
}

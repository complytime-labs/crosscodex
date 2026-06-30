package telemetrytest_test

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
)

func TestTelemetryTestBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "TelemetryTest BDD Suite")
}

var _ = BeforeSuite(func() { DeferCleanup(testspecs.RedirectLogsToGinkgo()) })

var _ = Describe("Telemetry Test Assertions", Ordered, func() {

	Describe("FindSpan", func() {
		var tp *telemetrytest.TestProvider

		BeforeEach(func() {
			var err error
			tp, err = telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(tp.Shutdown, context.Background())
		})

		It("finds a span by name", func() {
			tracer := tp.TracerProvider().Tracer("test")
			ctx := context.Background()

			_, span := tracer.Start(ctx, "op.Alpha")
			span.End()
			_, span2 := tracer.Start(ctx, "op.Beta")
			span2.End()

			spans := tp.GetSpans()
			found := telemetrytest.FindSpan(spans, "op.Alpha")
			Expect(found).NotTo(BeNil())
			Expect(found.Name()).To(Equal("op.Alpha"))
		})

		It("returns nil for a nonexistent span name", func() {
			tracer := tp.TracerProvider().Tracer("test")
			ctx := context.Background()

			_, span := tracer.Start(ctx, "op.Alpha")
			span.End()

			spans := tp.GetSpans()
			notFound := telemetrytest.FindSpan(spans, "op.Gamma")
			Expect(notFound).To(BeNil())
		})
	})

	Describe("FindSpans", func() {
		var tp *telemetrytest.TestProvider

		BeforeEach(func() {
			var err error
			tp, err = telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(tp.Shutdown, context.Background())
		})

		It("finds all spans matching a name", func() {
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
			Expect(matches).To(HaveLen(3))
		})
	})

	Describe("SpanAttribute", func() {
		var tp *telemetrytest.TestProvider

		BeforeEach(func() {
			var err error
			tp, err = telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(tp.Shutdown, context.Background())
		})

		It("extracts an existing attribute by key", func() {
			tracer := tp.TracerProvider().Tracer("test")
			ctx := context.Background()

			_, span := tracer.Start(ctx, "op.WithAttrs",
				trace.WithAttributes(attribute.String("tenant.id", "acme")))
			span.End()

			spans := tp.GetSpans()
			found := telemetrytest.FindSpan(spans, "op.WithAttrs")
			Expect(found).NotTo(BeNil())

			val, ok := telemetrytest.SpanAttribute(found, "tenant.id")
			Expect(ok).To(BeTrue())
			Expect(val.AsString()).To(Equal("acme"))
		})

		It("returns false for a nonexistent attribute", func() {
			tracer := tp.TracerProvider().Tracer("test")
			ctx := context.Background()

			_, span := tracer.Start(ctx, "op.WithAttrs",
				trace.WithAttributes(attribute.String("tenant.id", "acme")))
			span.End()

			spans := tp.GetSpans()
			found := telemetrytest.FindSpan(spans, "op.WithAttrs")
			Expect(found).NotTo(BeNil())

			_, ok := telemetrytest.SpanAttribute(found, "nonexistent")
			Expect(ok).To(BeFalse())
		})
	})

	Describe("FindMetric and CounterValue", func() {
		var tp *telemetrytest.TestProvider

		BeforeEach(func() {
			var err error
			tp, err = telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(tp.Shutdown, context.Background())
		})

		It("finds a counter metric and reads its value", func() {
			meter := tp.MeterProvider().Meter("test")
			counter, err := meter.Int64Counter("test.ops.total")
			Expect(err).NotTo(HaveOccurred())
			counter.Add(context.Background(), 5)

			rm := tp.GetMetrics()
			m := telemetrytest.FindMetric(rm, "test.ops.total")
			Expect(m).NotTo(BeNil())

			val, err := telemetrytest.CounterValue(m)
			Expect(err).NotTo(HaveOccurred())
			Expect(val).To(Equal(int64(5)))
		})
	})

	Describe("FindMetric and HistogramCount", func() {
		var tp *telemetrytest.TestProvider

		BeforeEach(func() {
			var err error
			tp, err = telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(tp.Shutdown, context.Background())
		})

		It("finds a histogram metric and reads its count", func() {
			meter := tp.MeterProvider().Meter("test")
			hist, err := meter.Int64Histogram("test.duration_ms")
			Expect(err).NotTo(HaveOccurred())
			hist.Record(context.Background(), 42)
			hist.Record(context.Background(), 100)

			rm := tp.GetMetrics()
			m := telemetrytest.FindMetric(rm, "test.duration_ms")
			Expect(m).NotTo(BeNil())

			count, err := telemetrytest.HistogramCount(m)
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(Equal(uint64(2)))
		})
	})

	Describe("CounterValue with wrong metric type", func() {
		var tp *telemetrytest.TestProvider

		BeforeEach(func() {
			var err error
			tp, err = telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(tp.Shutdown, context.Background())
		})

		It("returns an error when passed a histogram", func() {
			meter := tp.MeterProvider().Meter("test")
			hist, err := meter.Int64Histogram("test.wrong_type")
			Expect(err).NotTo(HaveOccurred())
			hist.Record(context.Background(), 1)

			rm := tp.GetMetrics()
			m := telemetrytest.FindMetric(rm, "test.wrong_type")
			Expect(m).NotTo(BeNil())

			_, err = telemetrytest.CounterValue(m)
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("Resource service.name", func() {
		It("defaults to crosscodex-test when no WithServiceName is given", func() {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(tp.Shutdown, context.Background())

			tracer := tp.TracerProvider().Tracer("test")
			_, span := tracer.Start(context.Background(), "svc-name-check")
			span.End()

			spans := tp.GetSpans()
			Expect(spans).To(HaveLen(1))

			res := spans[0].Resource()
			val, ok := res.Set().Value("service.name")
			Expect(ok).To(BeTrue(), "resource should have service.name attribute")
			Expect(val.AsString()).To(Equal("crosscodex-test"))
		})

		It("uses the name from WithServiceName", func() {
			tp, err := telemetrytest.NewTestProvider(
				telemetrytest.WithServiceName("my-catalog-service"),
			)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(tp.Shutdown, context.Background())

			tracer := tp.TracerProvider().Tracer("test")
			_, span := tracer.Start(context.Background(), "svc-name-check")
			span.End()

			spans := tp.GetSpans()
			Expect(spans).To(HaveLen(1))

			res := spans[0].Resource()
			val, ok := res.Set().Value("service.name")
			Expect(ok).To(BeTrue(), "resource should have service.name attribute")
			Expect(val.AsString()).To(Equal("my-catalog-service"))
		})
	})

	Describe("GaugeValue", func() {
		var tp *telemetrytest.TestProvider

		BeforeEach(func() {
			var err error
			tp, err = telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(tp.Shutdown, context.Background())
		})

		It("reads the value of a gauge metric", func() {
			meter := tp.MeterProvider().Meter("test")
			gauge, err := meter.Int64Gauge("test.gauge")
			Expect(err).NotTo(HaveOccurred())
			gauge.Record(context.Background(), 7)

			rm := tp.GetMetrics()
			m := telemetrytest.FindMetric(rm, "test.gauge")
			Expect(m).NotTo(BeNil())

			val, err := telemetrytest.GaugeValue(m)
			Expect(err).NotTo(HaveOccurred())
			Expect(val).To(Equal(int64(7)))
		})

		It("returns an error when passed a counter instead of a gauge", func() {
			meter := tp.MeterProvider().Meter("test")
			counter, err := meter.Int64Counter("test.not_gauge")
			Expect(err).NotTo(HaveOccurred())
			counter.Add(context.Background(), 1)

			rm := tp.GetMetrics()
			m := telemetrytest.FindMetric(rm, "test.not_gauge")
			Expect(m).NotTo(BeNil())

			_, err = telemetrytest.GaugeValue(m)
			Expect(err).To(HaveOccurred())
		})
	})
})

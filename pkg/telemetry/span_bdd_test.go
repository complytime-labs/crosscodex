package telemetry_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/complytime-labs/crosscodex/pkg/telemetry"
)

var _ = Describe("StartSpan", func() {
	Context("when tracer is nil", func() {
		It("returns the original context and a non-recording span", func() {
			ctx := context.Background()
			newCtx, span := telemetry.StartSpan(nil, ctx, "test.operation")
			Expect(newCtx).To(Equal(ctx))
			Expect(span.IsRecording()).To(BeFalse())
			span.End()
		})
	})

	Context("when tracer is provided", func() {
		It("creates and returns a new span", func() {
			exporter := tracetest.NewInMemoryExporter()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
			defer func() { _ = tp.Shutdown(context.Background()) }()
			tracer := tp.Tracer("test")

			ctx := context.Background()
			newCtx, span := telemetry.StartSpan(tracer, ctx, "test.operation")
			Expect(newCtx).NotTo(Equal(ctx))
			Expect(span.IsRecording()).To(BeTrue())
			span.End()

			spans := exporter.GetSpans()
			Expect(spans).To(HaveLen(1))
			Expect(spans[0].Name).To(Equal("test.operation"))
		})
	})

	Context("when tracer is a noop tracer", func() {
		It("returns a non-recording span without error", func() {
			tracer := noop.NewTracerProvider().Tracer("noop")
			ctx := context.Background()
			_, span := telemetry.StartSpan(tracer, ctx, "test.operation")
			Expect(span.IsRecording()).To(BeFalse())
			span.End()
		})
	})
})

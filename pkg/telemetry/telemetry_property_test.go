package telemetry_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
)

// Suite bootstrap lives in telemetry_bdd_test.go — do NOT add RunSpecs here.

var _ = Describe("Property Specifications", Ordered, func() {
	Context("resolveEndpoint — signal override semantics", func() {
		It("non-empty signal endpoint always wins", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				signal := rapid.StringN(1, 50, -1).Draw(t, "signal")
				shared := rapid.String().Draw(t, "shared")
				result := telemetry.ResolveEndpoint(signal, shared)
				if result != signal {
					t.Fatalf("signal endpoint should win: got %q, want %q", result, signal)
				}
			})
		})

		It("empty signal falls back to shared", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				shared := rapid.String().Draw(t, "shared")
				result := telemetry.ResolveEndpoint("", shared)
				if result != shared {
					t.Fatalf("should fall back to shared: got %q, want %q", result, shared)
				}
			})
		})
	})

	Context("resolveProtocol — default to grpc", func() {
		It("both empty returns grpc", func() {
			result := telemetry.ResolveProtocol("", "")
			Expect(result).To(Equal("grpc"))
		})

		It("non-empty signal protocol always wins", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				signal := rapid.SampledFrom([]string{"grpc", "http"}).Draw(t, "signal")
				shared := rapid.String().Draw(t, "shared")
				result := telemetry.ResolveProtocol(signal, shared)
				if result != signal {
					t.Fatalf("signal protocol should win: got %q, want %q", result, signal)
				}
			})
		})
	})

	Context("Instrumentation — tracer scope naming", func() {
		It("names the tracer scope crosscodex/<component> for any component path", func() {
			DeferCleanup(testspecs.IsolateTelemetryGlobals())
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(tp.Shutdown, context.Background())
			otel.SetTracerProvider(tp.TracerProvider())
			otel.SetMeterProvider(tp.MeterProvider())

			rapid.Check(GinkgoT(), func(t *rapid.T) {
				component := rapid.StringMatching(`[a-z]+(/[a-z]+)*`).Draw(t, "component")
				tp.Reset()

				tracer, _ := telemetry.Instrumentation(component)
				_, span := tracer.Start(context.Background(), "op")
				span.End()

				spans := tp.GetSpans()
				if len(spans) != 1 {
					t.Fatalf("expected 1 span, got %d", len(spans))
				}
				want := "crosscodex/" + component
				if got := spans[0].InstrumentationScope().Name; got != want {
					t.Fatalf("scope name = %q, want %q", got, want)
				}
			})
		})
	})
})

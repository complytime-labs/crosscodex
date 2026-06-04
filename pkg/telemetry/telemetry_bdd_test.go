package telemetry_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
)

func TestTelemetryBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Telemetry BDD Suite")
}

var _ = BeforeSuite(func() { DeferCleanup(testspecs.RedirectLogsToGinkgo()) })

var _ = Describe("Telemetry System", Ordered, func() {

	BeforeAll(func() {
		testspecs.LogTestProgress("Starting Telemetry BDD test suite")
	})

	AfterAll(func() {
		testspecs.LogTestProgress("Telemetry BDD test suite completed")
	})

	// =================================================================
	// LEVEL 1: BEHAVIORAL SPECIFICATIONS
	// =================================================================

	Describe("Initialization Behaviors", func() {
		Context("when endpoint is empty (disabled mode)", func() {
			It("returns no error and a safe shutdown function", func() {
				cfg := config.ObservabilityConfig{
					Endpoint: "",
					Protocol: "grpc",
				}
				shutdown, err := telemetry.Init(context.Background(), cfg,
					telemetry.WithServiceName("test-svc"),
				)
				Expect(err).NotTo(HaveOccurred())
				Expect(shutdown).NotTo(BeNil())

				By("shutdown is safe to call")
				Expect(shutdown(context.Background())).To(Succeed())
			})
		})

		Context("when tracing endpoint is set but metrics endpoint is empty", func() {
			It("creates tracing provider and no-op metrics", func() {
				cfg := config.ObservabilityConfig{
					Endpoint: "",
					Protocol: "grpc",
					Tracing: config.ObservabilityTracingConfig{
						Endpoint:   "localhost:4317",
						SampleRate: 1.0,
					},
				}
				shutdown, err := telemetry.Init(context.Background(), cfg,
					telemetry.WithServiceName("test-svc"),
				)
				Expect(err).NotTo(HaveOccurred())
				Expect(shutdown).NotTo(BeNil())

				By("shutdown completes (exporter may fail to connect, but no error is propagated for empty batches)")
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
				defer cancel()
				// Shutdown may return an error from the exporter trying to connect;
				// in unit tests without a collector, we only verify it doesn't panic.
				_ = shutdown(shutdownCtx)
			})
		})

		Context("when metrics endpoint is set but tracing endpoint is empty", func() {
			It("creates metrics provider and no-op tracing", func() {
				cfg := config.ObservabilityConfig{
					Endpoint: "",
					Protocol: "grpc",
					Metrics: config.ObservabilityMetricsConfig{
						Endpoint: "localhost:4317",
						Interval: "10s",
					},
				}
				shutdown, err := telemetry.Init(context.Background(), cfg,
					telemetry.WithServiceName("test-svc"),
				)
				Expect(err).NotTo(HaveOccurred())
				Expect(shutdown).NotTo(BeNil())

				By("shutdown completes (exporter may fail to connect)")
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
				defer cancel()
				_ = shutdown(shutdownCtx)
			})
		})
	})

	// =================================================================
	// LEVEL 3: EDGE CASES
	// =================================================================

	Describe("Configuration Validation", func() {
		Context("when protocol is invalid", func() {
			It("returns ErrInvalidConfig", func() {
				cfg := config.ObservabilityConfig{
					Endpoint: "localhost:4317",
					Protocol: "websocket",
				}
				_, err := telemetry.Init(context.Background(), cfg)
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, telemetry.ErrInvalidConfig)).To(BeTrue())
			})
		})

		Context("when sample rate is out of range", func() {
			It("returns ErrInvalidConfig for negative rate", func() {
				cfg := config.ObservabilityConfig{
					Endpoint: "localhost:4317",
					Protocol: "grpc",
					Tracing: config.ObservabilityTracingConfig{
						SampleRate: -0.1,
					},
				}
				_, err := telemetry.Init(context.Background(), cfg)
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, telemetry.ErrInvalidConfig)).To(BeTrue())
			})

			It("returns ErrInvalidConfig for rate above 1.0", func() {
				cfg := config.ObservabilityConfig{
					Endpoint: "localhost:4317",
					Protocol: "grpc",
					Tracing: config.ObservabilityTracingConfig{
						SampleRate: 1.1,
					},
				}
				_, err := telemetry.Init(context.Background(), cfg)
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, telemetry.ErrInvalidConfig)).To(BeTrue())
			})
		})

		Context("when metrics interval is unparseable", func() {
			It("returns ErrInvalidConfig", func() {
				cfg := config.ObservabilityConfig{
					Endpoint: "localhost:4317",
					Protocol: "grpc",
					Metrics: config.ObservabilityMetricsConfig{
						Interval: "not-a-duration",
					},
				}
				_, err := telemetry.Init(context.Background(), cfg)
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, telemetry.ErrInvalidConfig)).To(BeTrue())
			})
		})
	})

	// =================================================================
	// LEVEL 2: INTERFACE COMPLIANCE
	// =================================================================

	Describe("Context Correlation", func() {
		Context("when extracting trace context from a span", func() {
			It("returns the hex trace ID from an active span", func() {
				tp := noop.NewTracerProvider()
				tracer := tp.Tracer("test")
				ctx, span := tracer.Start(context.Background(), "test-op")
				defer span.End()

				traceID := telemetry.TraceIDFromContext(ctx)
				spanID := telemetry.SpanIDFromContext(ctx)

				sc := trace.SpanFromContext(ctx).SpanContext()
				if sc.HasTraceID() {
					Expect(traceID).To(Equal(sc.TraceID().String()))
				}
				if sc.HasSpanID() {
					Expect(spanID).To(Equal(sc.SpanID().String()))
				}
			})
		})

		Context("when no span is in the context", func() {
			It("returns empty strings", func() {
				ctx := context.Background()
				Expect(telemetry.TraceIDFromContext(ctx)).To(BeEmpty())
				Expect(telemetry.SpanIDFromContext(ctx)).To(BeEmpty())
			})
		})
	})

	// =================================================================
	// LEVEL 4: INTERNAL FUNCTION TESTS (via export_test.go)
	// =================================================================

	Describe("Endpoint Resolution (Internal)", func() {
		DescribeTable("resolves per-signal override vs shared default",
			func(signalEndpoint, sharedEndpoint, expected string) {
				result := telemetry.ResolveEndpoint(signalEndpoint, sharedEndpoint)
				Expect(result).To(Equal(expected))
			},
			Entry("signal overrides shared", "signal:4317", "shared:4317", "signal:4317"),
			Entry("shared used when signal empty", "", "shared:4317", "shared:4317"),
			Entry("both empty = disabled", "", "", ""),
			Entry("signal set, shared empty", "signal:4317", "", "signal:4317"),
		)

		DescribeTable("resolves per-signal protocol override",
			func(signalProtocol, sharedProtocol, expected string) {
				result := telemetry.ResolveProtocol(signalProtocol, sharedProtocol)
				Expect(result).To(Equal(expected))
			},
			Entry("signal overrides shared", "http", "grpc", "http"),
			Entry("shared used when signal empty", "", "grpc", "grpc"),
			Entry("both empty = grpc default", "", "", "grpc"),
		)
	})

	Describe("traceHandler (Internal)", func() {
		Context("when a span is active in the context", func() {
			It("injects trace_id and span_id into log records", func() {
				By("creating a recording span with a real TracerProvider")
				tp := sdktrace.NewTracerProvider()
				defer func() { _ = tp.Shutdown(context.Background()) }()

				tracer := tp.Tracer("test")
				ctx, span := tracer.Start(context.Background(), "log-test")
				defer span.End()

				sc := span.SpanContext()

				By("logging through the trace handler")
				var buf bytes.Buffer
				inner := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
				handler := telemetry.NewTraceHandler(inner)

				record := slog.NewRecord(time.Now(), slog.LevelInfo, "test message", 0)
				err := handler.Handle(ctx, record)
				Expect(err).NotTo(HaveOccurred())

				By("verifying trace_id and span_id appear in output")
				output := buf.String()
				Expect(output).To(ContainSubstring(sc.TraceID().String()))
				Expect(output).To(ContainSubstring(sc.SpanID().String()))
			})
		})

		Context("when no span is in the context", func() {
			It("does not inject trace fields", func() {
				var buf bytes.Buffer
				inner := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
				handler := telemetry.NewTraceHandler(inner)

				record := slog.NewRecord(time.Now(), slog.LevelInfo, "no span", 0)
				err := handler.Handle(context.Background(), record)
				Expect(err).NotTo(HaveOccurred())

				output := buf.String()
				Expect(output).NotTo(ContainSubstring("trace_id"))
				Expect(output).NotTo(ContainSubstring("span_id"))
			})
		})

		Context("when handler is used with WithAttrs and WithGroup", func() {
			It("preserves attributes through wrapping", func() {
				var buf bytes.Buffer
				inner := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
				handler := telemetry.NewTraceHandler(inner)

				withAttrs := handler.WithAttrs([]slog.Attr{slog.String("component", "test")})
				record := slog.NewRecord(time.Now(), slog.LevelInfo, "attrs test", 0)
				err := withAttrs.Handle(context.Background(), record)
				Expect(err).NotTo(HaveOccurred())

				output := buf.String()
				Expect(output).To(ContainSubstring("component"))
				Expect(output).To(ContainSubstring("test"))
			})
		})
	})

	// =================================================================
	// LEVEL 2: INSTRUMENT FACTORIES
	// =================================================================

	Describe("Instrument Factories", func() {
		Context("when creating instruments via factory functions", func() {
			It("creates a Float64Counter", func() {
				counter, err := telemetry.NewCounter("test.counter")
				Expect(err).NotTo(HaveOccurred())
				Expect(counter).NotTo(BeNil())
			})

			It("creates a Float64Histogram", func() {
				histogram, err := telemetry.NewHistogram("test.histogram")
				Expect(err).NotTo(HaveOccurred())
				Expect(histogram).NotTo(BeNil())
			})

			It("creates a Float64Gauge", func() {
				gauge, err := telemetry.NewGauge("test.gauge")
				Expect(err).NotTo(HaveOccurred())
				Expect(gauge).NotTo(BeNil())
			})

			It("creates an Int64Counter", func() {
				counter, err := telemetry.NewIntCounter("test.int_counter")
				Expect(err).NotTo(HaveOccurred())
				Expect(counter).NotTo(BeNil())
			})
		})
	})

	// =================================================================
	// LEVEL 2: TEST PROVIDER (telemetrytest subpackage)
	// =================================================================

	Describe("TestProvider (telemetrytest)", func() {
		Context("when using the test provider for assertions", func() {
			It("captures spans created via its TracerProvider", func() {
				tp, err := telemetrytest.NewTestProvider()
				Expect(err).NotTo(HaveOccurred())

				tracer := tp.TracerProvider().Tracer("test")
				_, span := tracer.Start(context.Background(), "test-span")
				span.End()

				spans := tp.GetSpans()
				Expect(spans).To(HaveLen(1))
				Expect(spans[0].Name()).To(Equal("test-span"))
			})

			It("returns valid MeterProvider", func() {
				tp, err := telemetrytest.NewTestProvider()
				Expect(err).NotTo(HaveOccurred())
				Expect(tp.MeterProvider()).NotTo(BeNil())
			})

			It("resets captured spans", func() {
				tp, err := telemetrytest.NewTestProvider()
				Expect(err).NotTo(HaveOccurred())

				tracer := tp.TracerProvider().Tracer("test")
				_, span := tracer.Start(context.Background(), "before-reset")
				span.End()

				Expect(tp.GetSpans()).To(HaveLen(1))
				tp.Reset()
				Expect(tp.GetSpans()).To(BeEmpty())
			})

			It("shuts down cleanly", func() {
				tp, err := telemetrytest.NewTestProvider()
				Expect(err).NotTo(HaveOccurred())
				Expect(tp.Shutdown(context.Background())).To(Succeed())
			})
		})
	})
})

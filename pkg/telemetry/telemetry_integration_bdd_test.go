//go:build integration_telemetry

package telemetry_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"go.opentelemetry.io/otel"
)

// Suite bootstrap lives in telemetry_bdd_test.go — do NOT add RunSpecs or BeforeSuite here.

// assertServiceInJaeger queries the Jaeger API and asserts the service exists.
func assertServiceInJaeger(queryURL, serviceName string) {
	var found bool
	for attempt := 0; attempt < 30; attempt++ {
		time.Sleep(500 * time.Millisecond)

		resp, err := http.Get(fmt.Sprintf("%s/api/services", queryURL)) //nolint:gosec // test-only Jaeger query
		if err != nil {
			GinkgoLogr.Info("query failed", "attempt", attempt, "error", err)
			continue
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			GinkgoLogr.Info("read failed", "attempt", attempt, "error", err)
			continue
		}

		var result struct {
			Data []string `json:"data"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			GinkgoLogr.Info("parse failed", "attempt", attempt, "error", err)
			continue
		}

		for _, svc := range result.Data {
			if svc == serviceName {
				found = true
				break
			}
		}
		if found {
			break
		}
	}

	Expect(found).To(BeTrue(), "service %q not found in Jaeger after 15s", serviceName)
}

var _ = Describe("Telemetry Integration", Ordered, func() {

	Describe("OTLP gRPC Export", func() {
		var (
			endpoint string
			queryURL string
		)

		BeforeEach(func() {
			endpoint = os.Getenv("TEST_OTLP_GRPC_ENDPOINT")
			if endpoint == "" {
				Skip("TEST_OTLP_GRPC_ENDPOINT not set")
			}
			queryURL = os.Getenv("TEST_JAEGER_QUERY_URL")
			if queryURL == "" {
				Skip("TEST_JAEGER_QUERY_URL not set")
			}
		})

		It("exports traces to Jaeger via gRPC", func() {
			ctx := context.Background()
			serviceName := fmt.Sprintf("test-grpc-%d", time.Now().UnixNano())

			cfg := config.ObservabilityConfig{
				Protocol: "grpc",
				Tracing: config.ObservabilityTracingConfig{
					Endpoint:   endpoint,
					SampleRate: 1.0,
				},
			}

			shutdown, err := telemetry.Init(ctx, cfg,
				telemetry.WithServiceName(serviceName),
				telemetry.WithServiceVersion("test"),
			)
			Expect(err).NotTo(HaveOccurred())

			tracer := otel.GetTracerProvider().Tracer("integration-test")
			_, span := tracer.Start(ctx, "test-operation")
			span.End()

			Expect(shutdown(ctx)).To(Succeed())

			assertServiceInJaeger(queryURL, serviceName)
		})
	})

	Describe("OTLP HTTP Export", func() {
		var (
			endpoint string
			queryURL string
		)

		BeforeEach(func() {
			endpoint = os.Getenv("TEST_OTLP_HTTP_ENDPOINT")
			if endpoint == "" {
				Skip("TEST_OTLP_HTTP_ENDPOINT not set")
			}
			queryURL = os.Getenv("TEST_JAEGER_QUERY_URL")
			if queryURL == "" {
				Skip("TEST_JAEGER_QUERY_URL not set")
			}
		})

		It("exports traces to Jaeger via HTTP", func() {
			ctx := context.Background()
			serviceName := fmt.Sprintf("test-http-%d", time.Now().UnixNano())

			cfg := config.ObservabilityConfig{
				Protocol: "http",
				Tracing: config.ObservabilityTracingConfig{
					Endpoint:   endpoint,
					SampleRate: 1.0,
				},
			}

			shutdown, err := telemetry.Init(ctx, cfg,
				telemetry.WithServiceName(serviceName),
				telemetry.WithServiceVersion("test"),
			)
			Expect(err).NotTo(HaveOccurred())

			tracer := otel.GetTracerProvider().Tracer("integration-test")
			_, span := tracer.Start(ctx, "test-http-operation")
			span.End()

			Expect(shutdown(ctx)).To(Succeed())

			assertServiceInJaeger(queryURL, serviceName)
		})
	})

	Describe("Slog Trace ID Correlation", func() {
		BeforeEach(func() {
			endpoint := os.Getenv("TEST_OTLP_GRPC_ENDPOINT")
			if endpoint == "" {
				Skip("TEST_OTLP_GRPC_ENDPOINT not set")
			}
		})

		It("produces non-empty trace and span IDs from an active span", func() {
			endpoint := os.Getenv("TEST_OTLP_GRPC_ENDPOINT")
			ctx := context.Background()

			cfg := config.ObservabilityConfig{
				Protocol: "grpc",
				Tracing: config.ObservabilityTracingConfig{
					Endpoint:   endpoint,
					SampleRate: 1.0,
				},
			}

			shutdown, err := telemetry.Init(ctx, cfg,
				telemetry.WithServiceName("slog-trace-test"),
			)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = shutdown(ctx) })

			tracer := otel.GetTracerProvider().Tracer("slog-test")
			ctx, span := tracer.Start(ctx, "slog-operation")
			DeferCleanup(span.End)

			traceID := telemetry.TraceIDFromContext(ctx)
			Expect(traceID).NotTo(BeEmpty(), "expected non-empty trace ID from active span")

			spanID := telemetry.SpanIDFromContext(ctx)
			Expect(spanID).NotTo(BeEmpty(), "expected non-empty span ID from active span")
		})
	})
})

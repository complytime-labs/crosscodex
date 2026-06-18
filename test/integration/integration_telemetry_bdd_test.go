//go:build integration_telemetry

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
)

func TestIntegrationTelemetryBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Telemetry Integration Suite")
}

var _ = Describe("Jaeger Export Smoke Test", func() {
	It("should export spans to Jaeger via OTLP", func() {
		otlpEndpoint := os.Getenv("TEST_OTLP_GRPC_ENDPOINT")
		if otlpEndpoint == "" {
			Skip("TEST_OTLP_GRPC_ENDPOINT not set; skipping Jaeger smoke test")
		}
		jaegerQueryURL := os.Getenv("TEST_JAEGER_QUERY_URL")
		if jaegerQueryURL == "" {
			Skip("TEST_JAEGER_QUERY_URL not set; skipping Jaeger smoke test")
		}

		ctx := context.Background()
		serviceName := "crosscodex-smoke-test"

		// Initialize real telemetry with OTLP gRPC export.
		cfg := config.ObservabilityConfig{
			Endpoint: otlpEndpoint,
			Protocol: "grpc",
			Tracing: config.ObservabilityTracingConfig{
				SampleRate: 1.0,
			},
		}

		shutdown, err := telemetry.Init(ctx, cfg,
			telemetry.WithServiceName(serviceName),
			telemetry.WithServiceVersion("test"),
		)
		Expect(err).NotTo(HaveOccurred(), "telemetry.Init")
		DeferCleanup(func() {
			// Flush by calling shutdown.
			if shutdownErr := shutdown(ctx); shutdownErr != nil {
				GinkgoWriter.Printf("telemetry shutdown (non-fatal): %v\n", shutdownErr)
			}
		})

		// Create a span using the global provider (proves global registration works).
		tracer := otel.GetTracerProvider().Tracer("smoke-test")
		_, span := tracer.Start(ctx, "smoke.test.span")
		span.End()

		// Flush by calling shutdown — this forces the span exporter to drain.
		if shutdownErr := shutdown(ctx); shutdownErr != nil {
			GinkgoWriter.Printf("telemetry shutdown (non-fatal): %v\n", shutdownErr)
		}

		// Query Jaeger for the service with retry loop.
		var found bool
		for attempt := 0; attempt < 10; attempt++ {
			time.Sleep(2 * time.Second)

			queryURL := fmt.Sprintf("%s/api/traces?service=%s&limit=10", jaegerQueryURL, serviceName)
			req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, queryURL, nil)
			if reqErr != nil {
				GinkgoWriter.Printf("attempt %d: request build error: %v\n", attempt+1, reqErr)
				continue
			}
			resp, respErr := http.DefaultClient.Do(req)
			if respErr != nil {
				GinkgoWriter.Printf("attempt %d: Jaeger query failed: %v\n", attempt+1, respErr)
				continue
			}

			body, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil {
				GinkgoWriter.Printf("attempt %d: read body failed: %v\n", attempt+1, readErr)
				continue
			}

			var result struct {
				Data []json.RawMessage `json:"data"`
			}
			if unmarshalErr := json.Unmarshal(body, &result); unmarshalErr != nil {
				GinkgoWriter.Printf("attempt %d: JSON parse failed: %v\n", attempt+1, unmarshalErr)
				continue
			}

			if len(result.Data) > 0 {
				found = true
				GinkgoWriter.Printf("Found %d trace(s) in Jaeger for service %q\n", len(result.Data), serviceName)
				break
			}
			GinkgoWriter.Printf("attempt %d: no traces found yet\n", attempt+1)
		}

		Expect(found).To(BeTrue(), "no traces found in Jaeger after 20s; OTLP export may be broken")
	})
})

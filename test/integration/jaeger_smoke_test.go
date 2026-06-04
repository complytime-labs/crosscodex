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

	"go.opentelemetry.io/otel"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
)

// TestJaegerExportSmokeTest verifies that spans emitted by non-telemetry
// packages actually reach a real Jaeger instance via OTLP export.
//
// Requires:
//   - Jaeger container running (compose telemetry profile)
//   - TEST_OTLP_GRPC_ENDPOINT env var set
//   - TEST_JAEGER_QUERY_URL env var set
func TestJaegerExportSmokeTest(t *testing.T) {
	otlpEndpoint := os.Getenv("TEST_OTLP_GRPC_ENDPOINT")
	if otlpEndpoint == "" {
		t.Skip("TEST_OTLP_GRPC_ENDPOINT not set; skipping Jaeger smoke test")
	}
	jaegerQueryURL := os.Getenv("TEST_JAEGER_QUERY_URL")
	if jaegerQueryURL == "" {
		t.Skip("TEST_JAEGER_QUERY_URL not set; skipping Jaeger smoke test")
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
	if err != nil {
		t.Fatalf("telemetry.Init: %v", err)
	}
	defer shutdown(ctx)

	// Create a span using the global provider (proves global registration works).
	tracer := otel.GetTracerProvider().Tracer("smoke-test")
	_, span := tracer.Start(ctx, "smoke.test.span")
	span.End()

	// Flush by calling shutdown — this forces the span exporter to drain.
	if err := shutdown(ctx); err != nil {
		t.Fatalf("telemetry shutdown: %v", err)
	}

	// Query Jaeger for the service with retry loop.
	var found bool
	for attempt := 0; attempt < 10; attempt++ {
		time.Sleep(2 * time.Second)

		url := fmt.Sprintf("%s/api/traces?service=%s&limit=10", jaegerQueryURL, serviceName)
		resp, err := http.Get(url)
		if err != nil {
			t.Logf("attempt %d: Jaeger query failed: %v", attempt+1, err)
			continue
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Logf("attempt %d: read body failed: %v", attempt+1, err)
			continue
		}

		var result struct {
			Data []json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			t.Logf("attempt %d: JSON parse failed: %v", attempt+1, err)
			continue
		}

		if len(result.Data) > 0 {
			found = true
			t.Logf("Found %d trace(s) in Jaeger for service %q", len(result.Data), serviceName)
			break
		}
		t.Logf("attempt %d: no traces found yet", attempt+1)
	}

	if !found {
		t.Fatal("no traces found in Jaeger after 20s; OTLP export may be broken")
	}
}

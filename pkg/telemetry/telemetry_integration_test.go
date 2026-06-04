//go:build integration_telemetry

package telemetry_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"go.opentelemetry.io/otel"
)

func TestIntegrationOTLPgRPC(t *testing.T) {
	endpoint := os.Getenv("TEST_OTLP_GRPC_ENDPOINT")
	if endpoint == "" {
		t.Skip("TEST_OTLP_GRPC_ENDPOINT not set")
	}
	queryURL := os.Getenv("TEST_JAEGER_QUERY_URL")
	if queryURL == "" {
		t.Skip("TEST_JAEGER_QUERY_URL not set")
	}

	ctx := context.Background()
	serviceName := fmt.Sprintf("test-grpc-%d", time.Now().UnixNano())

	// Use per-signal endpoint so only traces go to Jaeger.
	// Jaeger does not implement the OTLP MetricsService, so
	// pointing metrics at it causes shutdown errors.
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
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	// Create and complete a span
	tracer := otel.GetTracerProvider().Tracer("integration-test")
	_, span := tracer.Start(ctx, "test-operation")
	span.End()

	// Shutdown to flush
	if err := shutdown(ctx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}

	// Query Jaeger for the service
	assertServiceInJaeger(t, queryURL, serviceName)
}

func TestIntegrationOTLPHTTP(t *testing.T) {
	endpoint := os.Getenv("TEST_OTLP_HTTP_ENDPOINT")
	if endpoint == "" {
		t.Skip("TEST_OTLP_HTTP_ENDPOINT not set")
	}
	queryURL := os.Getenv("TEST_JAEGER_QUERY_URL")
	if queryURL == "" {
		t.Skip("TEST_JAEGER_QUERY_URL not set")
	}

	ctx := context.Background()
	serviceName := fmt.Sprintf("test-http-%d", time.Now().UnixNano())

	// Use per-signal endpoint so only traces go to Jaeger.
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
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	tracer := otel.GetTracerProvider().Tracer("integration-test")
	_, span := tracer.Start(ctx, "test-http-operation")
	span.End()

	if err := shutdown(ctx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}

	assertServiceInJaeger(t, queryURL, serviceName)
}

func TestIntegrationSlogTraceID(t *testing.T) {
	endpoint := os.Getenv("TEST_OTLP_GRPC_ENDPOINT")
	if endpoint == "" {
		t.Skip("TEST_OTLP_GRPC_ENDPOINT not set")
	}

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
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	defer func() { _ = shutdown(ctx) }()

	tracer := otel.GetTracerProvider().Tracer("slog-test")
	ctx, span := tracer.Start(ctx, "slog-operation")
	defer span.End()

	traceID := telemetry.TraceIDFromContext(ctx)
	if traceID == "" {
		t.Fatal("expected non-empty trace ID from active span")
	}

	spanID := telemetry.SpanIDFromContext(ctx)
	if spanID == "" {
		t.Fatal("expected non-empty span ID from active span")
	}
}

// assertServiceInJaeger queries the Jaeger API and asserts the service exists.
func assertServiceInJaeger(t *testing.T, queryURL, serviceName string) {
	t.Helper()

	// Jaeger may take a moment to index traces
	var found bool
	for attempt := 0; attempt < 30; attempt++ {
		time.Sleep(500 * time.Millisecond)

		resp, err := http.Get(fmt.Sprintf("%s/api/services", queryURL))
		if err != nil {
			t.Logf("attempt %d: query failed: %v", attempt, err)
			continue
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Logf("attempt %d: read failed: %v", attempt, err)
			continue
		}

		var result struct {
			Data []string `json:"data"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			t.Logf("attempt %d: parse failed: %v", attempt, err)
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

	if !found {
		t.Fatalf("service %q not found in Jaeger after 15s", serviceName)
	}
}

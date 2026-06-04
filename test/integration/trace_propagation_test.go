//go:build integration

package integration_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

func TestMain(m *testing.M) {
	// This test uses embedded NATS (no container needed) but requires
	// no external dependencies. If future tests in this package need
	// PostgreSQL, gate on TEST_DATABASE_DSN here.
	os.Exit(m.Run())
}

// TestCrossServiceTracePropagation verifies that a single trace ID flows
// through publish and subscriber processing across NATS.
//
// Flow: parent span -> natsbus.Publish -> natsbus.process (subscriber)
// Assert: all spans share the same TraceID.
func TestCrossServiceTracePropagation(t *testing.T) {
	tp, err := telemetrytest.NewTestProvider()
	if err != nil {
		t.Fatalf("telemetrytest.NewTestProvider: %v", err)
	}
	t.Cleanup(func() { tp.Shutdown(context.Background()) })

	tracer := tp.TracerProvider().Tracer("trace-propagation-test")
	meter := tp.MeterProvider().Meter("trace-propagation-test")

	// Create embedded NATS client with telemetry.
	storeDir := filepath.Join(t.TempDir(), "nats-store")
	cfg := config.NATSConfig{
		URL: "", // embedded mode
		Embedded: config.NATSEmbeddedConfig{
			StoreDir: storeDir,
		},
		Streams: config.NATSStreamsConfig{
			AuditLLMRetention:    24 * time.Hour,
			AuditEventsRetention: 24 * time.Hour,
		},
	}
	client, err := natsbus.New(cfg, natsbus.WithTelemetry(tracer, meter))
	if err != nil {
		t.Fatalf("natsbus.New: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	// Set up tenant context (same pattern as natsbus integration helpers).
	ctx := context.Background()
	ctx, err = tenant.WithTenant(ctx, "test-tenant")
	if err != nil {
		t.Fatalf("tenant context: %v", err)
	}

	subject, err := natsbus.WorkSubject("test-tenant", natsbus.TaskClassify, "job-trace-prop")
	if err != nil {
		t.Fatalf("subject: %v", err)
	}

	// Create parent span.
	ctx, parentSpan := tracer.Start(ctx, "test.request")
	parentTraceID := parentSpan.SpanContext().TraceID()

	// Subscribe.
	type result struct {
		sc trace.SpanContext
	}
	resultCh := make(chan result, 1)
	sub, err := client.Subscribe(ctx, subject, func(handlerCtx context.Context, _ *natsbus.Message) error {
		resultCh <- result{sc: trace.SpanContextFromContext(handlerCtx)}
		return nil
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { sub.Unsubscribe() })

	time.Sleep(100 * time.Millisecond)

	// Publish.
	if err := client.Publish(ctx, subject, []byte(`{"test":"trace-propagation"}`)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	parentSpan.End()

	// Wait for handler.
	select {
	case r := <-resultCh:
		if !r.sc.IsValid() {
			t.Fatal("subscriber SpanContext is not valid")
		}
		if r.sc.TraceID() != parentTraceID {
			t.Errorf("subscriber trace ID = %s, want %s", r.sc.TraceID(), parentTraceID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for subscriber")
	}

	// Allow spans to flush.
	time.Sleep(200 * time.Millisecond)

	// Collect all spans and verify trace ID consistency.
	spans := tp.GetSpans()

	// Filter to spans that belong to the test trace. Spans from client
	// initialization (e.g. natsbus.CreateStream for audit streams) use
	// different trace IDs and are not part of the publish/subscribe flow.
	spanNames := make(map[string]bool)
	var traceSpanCount int
	for _, s := range spans {
		if s.SpanContext().TraceID() == parentTraceID {
			spanNames[s.Name()] = true
			traceSpanCount++
		}
	}

	// Assert expected spans are present in the test trace.
	expectedSpans := []string{"test.request", "natsbus.Publish", "natsbus.Subscribe", "natsbus.process"}
	for _, name := range expectedSpans {
		if !spanNames[name] {
			t.Errorf("expected span %q not found in collected spans for trace %s", name, parentTraceID)
		}
	}

	t.Logf("Collected %d spans in trace %s (of %d total)", traceSpanCount, parentTraceID, len(spans))
}

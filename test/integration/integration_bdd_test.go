//go:build integration

package integration_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

func TestIntegrationBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Cross-Service Integration Suite")
}

var _ = Describe("Cross-Service Trace Propagation", func() {
	It("should propagate a single trace ID through NATS publish and subscribe", func() {
		tp, err := telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { tp.Shutdown(context.Background()) })

		tracer := tp.TracerProvider().Tracer("trace-propagation-test")
		meter := tp.MeterProvider().Meter("trace-propagation-test")

		// Create embedded NATS client with telemetry.
		storeDir := filepath.Join(GinkgoT().TempDir(), "nats-store")
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
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { client.Close() })

		// Set up tenant context.
		ctx := context.Background()
		ctx, err = tenant.WithTenant(ctx, "test-tenant")
		Expect(err).NotTo(HaveOccurred())

		subject, err := natsbus.WorkSubject("test-tenant", natsbus.TaskClassify, "job-trace-prop")
		Expect(err).NotTo(HaveOccurred())

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
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { sub.Unsubscribe() })

		time.Sleep(100 * time.Millisecond)

		// Publish.
		Expect(client.Publish(ctx, subject, []byte(`{"test":"trace-propagation"}`))).To(Succeed())
		parentSpan.End()

		// Wait for handler.
		var r result
		Eventually(resultCh, 5*time.Second).Should(Receive(&r))
		Expect(r.sc.IsValid()).To(BeTrue(), "subscriber SpanContext is not valid")
		Expect(r.sc.TraceID()).To(Equal(parentTraceID))

		// Allow spans to flush.
		time.Sleep(200 * time.Millisecond)

		// Collect all spans and verify trace ID consistency.
		spans := tp.GetSpans()

		// Filter to spans that belong to the test trace.
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
			Expect(spanNames).To(HaveKey(name),
				"expected span %q not found in collected spans for trace %s", name, parentTraceID)
		}

		GinkgoWriter.Printf("Collected %d spans in trace %s (of %d total)\n", traceSpanCount, parentTraceID, len(spans))
	})
})

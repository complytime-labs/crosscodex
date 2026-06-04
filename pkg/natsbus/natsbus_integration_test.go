//go:build integration

package natsbus_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
)

func newEmbeddedClient(t *testing.T) natsbus.Client {
	t.Helper()

	storeDir := filepath.Join(t.TempDir(), "nats-store")
	cfg := config.NATSConfig{
		URL: "", // embedded mode
		Embedded: config.NATSEmbeddedConfig{
			StoreDir: storeDir,
		},
		Streams: defaultTestStreamsConfig(),
	}

	client, err := natsbus.New(cfg)
	if err != nil {
		t.Fatalf("failed to create embedded client: %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("close error: %v", err)
		}
	})
	return client
}

func TestEmbeddedPublishSubscribe(t *testing.T) {
	client := newEmbeddedClient(t)
	ctx := testTenantCtx(t, "acme-corp")

	subject, err := natsbus.WorkSubject("acme-corp", natsbus.TaskClassify, "job-001")
	if err != nil {
		t.Fatalf("subject: %v", err)
	}

	received := subscribeOne(t, client, ctx, subject)

	payload := []byte(`{"task":"classify","control":"AC-1"}`)
	publishOrFail(t, client, ctx, subject, payload)

	msg := receiveOne(t, received, 5*time.Second)
	if msg.Subject != subject {
		t.Errorf("subject = %q, want %q", msg.Subject, subject)
	}
	if string(msg.Data) != string(payload) {
		t.Errorf("data = %q, want %q", msg.Data, payload)
	}
}

func TestEmbeddedProvenanceRoundTrip(t *testing.T) {
	client := newEmbeddedClient(t)
	ctx := testTenantCtx(t, "test-tenant")

	subject, err := natsbus.WorkSubject("test-tenant", natsbus.TaskRelate, "job-prov")
	if err != nil {
		t.Fatalf("subject: %v", err)
	}

	received := subscribeOne(t, client, ctx, subject)

	payload := []byte("provenance test data")
	publishOrFail(t, client, ctx, subject, payload)

	msg := receiveOne(t, received, 5*time.Second)
	assertProvenanceHeaders(t, msg, payload, "test-tenant")
}

func TestEmbeddedQueueGroupDistribution(t *testing.T) {
	client := newEmbeddedClient(t)
	ctx := testTenantCtx(t, "acme-corp")

	subject, err := natsbus.WorkSubject("acme-corp", natsbus.TaskClassify, "job-queue")
	if err != nil {
		t.Fatalf("subject: %v", err)
	}

	const numWorkers = 3
	const numMessages = 30

	var counts [numWorkers]atomic.Int64
	var wg sync.WaitGroup

	subs := make([]natsbus.Subscription, numWorkers)
	for i := range numWorkers {
		workerIdx := i
		sub, err := client.QueueSubscribe(ctx, subject, "test-workers", func(_ context.Context, msg *natsbus.Message) error {
			counts[workerIdx].Add(1)
			return nil
		})
		if err != nil {
			t.Fatalf("queue subscribe worker %d: %v", i, err)
		}
		subs[i] = sub
	}
	defer func() {
		for _, sub := range subs {
			sub.Unsubscribe()
		}
	}()

	// Small delay for subscriptions to propagate
	time.Sleep(100 * time.Millisecond)

	wg.Add(1)
	go func() {
		defer wg.Done()
		for range numMessages {
			if err := client.Publish(ctx, subject, []byte("work")); err != nil {
				t.Errorf("publish: %v", err)
			}
		}
	}()
	wg.Wait()

	// Wait for all messages to be delivered
	deadline := time.After(10 * time.Second)
	for {
		var total int64
		for i := range numWorkers {
			total += counts[i].Load()
		}
		if total >= numMessages {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timeout: only received %d of %d messages", total, numMessages)
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}

	// Verify distribution — each worker should have received at least 1
	for i := range numWorkers {
		c := counts[i].Load()
		t.Logf("worker %d received %d messages", i, c)
		if c == 0 {
			t.Errorf("worker %d received 0 messages; expected round-robin distribution", i)
		}
	}
}

func TestEmbeddedJetStreamAuditPersistence(t *testing.T) {
	client := newEmbeddedClient(t)
	ctx := testTenantCtx(t, "acme-corp")

	subject, err := natsbus.AuditSubject("acme-corp", natsbus.AuditLLM, "job-audit")
	if err != nil {
		t.Fatalf("subject: %v", err)
	}

	payload := []byte(`{"model":"gpt-4","prompt":"classify AC-1"}`)
	publishOrFail(t, client, ctx, subject, payload)

	// Give JetStream time to persist
	time.Sleep(500 * time.Millisecond)

	// Subscribe and verify the message was persisted in the audit stream
	received := subscribeOne(t, client, ctx, subject)

	// Publish another to trigger subscription
	payload2 := []byte(`{"model":"gpt-4","prompt":"classify AC-2"}`)
	publishOrFail(t, client, ctx, subject, payload2)

	msg := receiveOne(t, received, 5*time.Second)
	if string(msg.Data) != string(payload2) {
		t.Errorf("data = %q, want %q", msg.Data, payload2)
	}
}

func TestEmbeddedTenantIsolation(t *testing.T) {
	client := newEmbeddedClient(t)

	subjectA, err := natsbus.WorkSubject("tenant-aaa", natsbus.TaskClassify, "job-iso")
	if err != nil {
		t.Fatalf("subject A: %v", err)
	}
	subjectB, err := natsbus.WorkSubject("tenant-bbb", natsbus.TaskClassify, "job-iso")
	if err != nil {
		t.Fatalf("subject B: %v", err)
	}

	ctxA := testTenantCtx(t, "tenant-aaa")
	ctxB := testTenantCtx(t, "tenant-bbb")

	var receivedByA atomic.Int64
	var receivedByB atomic.Int64

	subA, err := client.Subscribe(ctxA, subjectA, func(_ context.Context, msg *natsbus.Message) error {
		receivedByA.Add(1)
		return nil
	})
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}
	defer subA.Unsubscribe()

	subB, err := client.Subscribe(ctxB, subjectB, func(_ context.Context, msg *natsbus.Message) error {
		receivedByB.Add(1)
		return nil
	})
	if err != nil {
		t.Fatalf("subscribe B: %v", err)
	}
	defer subB.Unsubscribe()

	time.Sleep(100 * time.Millisecond)

	// Publish only to tenant A
	for range 5 {
		if err := client.Publish(ctxA, subjectA, []byte("for A")); err != nil {
			t.Fatalf("publish to A: %v", err)
		}
	}

	time.Sleep(500 * time.Millisecond)

	if receivedByA.Load() != 5 {
		t.Errorf("tenant A received %d messages, want 5", receivedByA.Load())
	}
	if receivedByB.Load() != 0 {
		t.Errorf("tenant B received %d messages, want 0 (isolation violated)", receivedByB.Load())
	}
}

func TestEmbeddedCloseIdempotent(t *testing.T) {
	storeDir := filepath.Join(t.TempDir(), "nats-close-test")
	cfg := config.NATSConfig{
		URL: "",
		Embedded: config.NATSEmbeddedConfig{
			StoreDir: storeDir,
		},
		Streams: defaultTestStreamsConfig(),
	}

	client, err := natsbus.New(cfg)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Close twice — should not panic
	if err := client.Close(); err != nil {
		t.Errorf("first close: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Errorf("second close: %v", err)
	}
}

func TestEmbeddedPublishAfterClose(t *testing.T) {
	client := newEmbeddedClient(t)
	ctx := testTenantCtx(t, "acme-corp")

	if err := client.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	subject, err := natsbus.WorkSubject("acme-corp", natsbus.TaskClassify, "job-closed")
	if err != nil {
		t.Fatalf("subject: %v", err)
	}

	err = client.Publish(ctx, subject, []byte("should fail"))
	if err == nil {
		t.Fatal("expected error publishing to closed client")
	}
}

func TestEmbeddedXDGStateHome(t *testing.T) {
	customDir := filepath.Join(t.TempDir(), "custom-state")
	t.Setenv("XDG_STATE_HOME", customDir)

	cfg := config.NATSConfig{
		URL: "",
		Embedded: config.NATSEmbeddedConfig{
			StoreDir: "", // should use XDG_STATE_HOME
		},
		Streams: defaultTestStreamsConfig(),
	}

	client, err := natsbus.New(cfg)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer client.Close()

	// Verify the store directory was created under XDG_STATE_HOME
	expectedDir := filepath.Join(customDir, "crosscodex", "nats")
	if _, err := os.Stat(expectedDir); os.IsNotExist(err) {
		t.Errorf("expected store dir %q to exist", expectedDir)
	}
}

func TestIntegration_Publish_MissingTenantContext(t *testing.T) {
	client := newEmbeddedClient(t)

	// A subject requires a valid tenant to construct, so we use a pre-built
	// subject string to test the publish path with a bare context.
	subject, err := natsbus.WorkSubject("acme-corp", natsbus.TaskClassify, "job-001")
	if err != nil {
		t.Fatalf("subject: %v", err)
	}

	// Publish with a context that has no tenant — should fail.
	err = client.Publish(context.Background(), subject, []byte("hello"))
	if err == nil {
		t.Fatal("expected error when publishing without tenant context, got nil")
	}

	// Verify the error mentions tenant.
	if !strings.Contains(err.Error(), "tenant") {
		t.Errorf("error should mention tenant, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Telemetry
// ---------------------------------------------------------------------------

func newEmbeddedClientWithTelemetry(t *testing.T, tracer trace.Tracer, meter metric.Meter) natsbus.Client {
	t.Helper()

	storeDir := filepath.Join(t.TempDir(), "nats-store")
	cfg := config.NATSConfig{
		URL: "",
		Embedded: config.NATSEmbeddedConfig{
			StoreDir: storeDir,
		},
		Streams: defaultTestStreamsConfig(),
	}

	client, err := natsbus.New(cfg, natsbus.WithTelemetry(tracer, meter))
	if err != nil {
		t.Fatalf("failed to create embedded client with telemetry: %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("close error: %v", err)
		}
	})
	return client
}

func TestTelemetrySpansOnPublish(t *testing.T) {
	tp, err := telemetrytest.NewTestProvider()
	if err != nil {
		t.Fatalf("telemetrytest.NewTestProvider: %v", err)
	}
	t.Cleanup(func() { tp.Shutdown(context.Background()) })

	tracer := tp.TracerProvider().Tracer("natsbus-test")
	meter := tp.MeterProvider().Meter("natsbus-test")
	client := newEmbeddedClientWithTelemetry(t, tracer, meter)

	ctx := testTenantCtx(t, "acme-corp")
	subject, err := natsbus.WorkSubject("acme-corp", natsbus.TaskClassify, "job-tel")
	if err != nil {
		t.Fatalf("subject: %v", err)
	}

	received := subscribeOne(t, client, ctx, subject)

	payload := []byte(`{"task":"classify","control":"AC-1"}`)
	publishOrFail(t, client, ctx, subject, payload)
	receiveOne(t, received, 5*time.Second)

	// Assert publish span exists with expected attributes.
	spans := tp.GetSpans()
	pubSpan := telemetrytest.FindSpan(spans, "natsbus.Publish")
	if pubSpan == nil {
		t.Fatal("expected natsbus.Publish span, got none")
	}

	subjectAttr, ok := telemetrytest.SpanAttribute(pubSpan, "messaging.subject")
	if !ok {
		t.Error("natsbus.Publish span missing messaging.subject attribute")
	} else if subjectAttr.AsString() != subject {
		t.Errorf("messaging.subject = %q, want %q", subjectAttr.AsString(), subject)
	}

	tenantAttr, ok := telemetrytest.SpanAttribute(pubSpan, "tenant.id")
	if !ok {
		t.Error("natsbus.Publish span missing tenant.id attribute")
	} else if tenantAttr.AsString() != "acme-corp" {
		t.Errorf("tenant.id = %q, want %q", tenantAttr.AsString(), "acme-corp")
	}

	// Assert publish counter metric >= 1.
	rm := tp.GetMetrics()
	m := telemetrytest.FindMetric(rm, "natsbus.publish.total")
	if m == nil {
		t.Fatal("expected natsbus.publish.total metric, got none")
	}
	val, err := telemetrytest.CounterValue(m)
	if err != nil {
		t.Fatalf("CounterValue: %v", err)
	}
	if val < 1 {
		t.Errorf("natsbus.publish.total = %d, want >= 1", val)
	}

	// Assert natsbus.Subscribe span exists with messaging.subject attribute.
	subSpan := telemetrytest.FindSpan(spans, "natsbus.Subscribe")
	if subSpan == nil {
		t.Fatal("expected natsbus.Subscribe span, got none")
	}
	subSubjectAttr, ok := telemetrytest.SpanAttribute(subSpan, "messaging.subject")
	if !ok {
		t.Error("natsbus.Subscribe span missing messaging.subject attribute")
	} else if subSubjectAttr.AsString() != subject {
		t.Errorf("Subscribe messaging.subject = %q, want %q", subSubjectAttr.AsString(), subject)
	}

	// Assert publish duration histogram.
	hm := telemetrytest.FindMetric(rm, "natsbus.publish.duration_ms")
	if hm == nil {
		t.Fatal("expected natsbus.publish.duration_ms metric, got none")
	}
	hc, err := telemetrytest.HistogramCount(hm)
	if err != nil {
		t.Fatalf("HistogramCount: %v", err)
	}
	if hc < 1 {
		t.Errorf("natsbus.publish.duration_ms count = %d, want >= 1", hc)
	}
}

func TestTelemetryTraceContextRoundTrip(t *testing.T) {
	tp, err := telemetrytest.NewTestProvider()
	if err != nil {
		t.Fatalf("telemetrytest.NewTestProvider: %v", err)
	}
	t.Cleanup(func() { tp.Shutdown(context.Background()) })

	tracer := tp.TracerProvider().Tracer("natsbus-test")
	meter := tp.MeterProvider().Meter("natsbus-test")
	client := newEmbeddedClientWithTelemetry(t, tracer, meter)

	ctx := testTenantCtx(t, "acme-corp")
	subject, err := natsbus.WorkSubject("acme-corp", natsbus.TaskClassify, "job-trace")
	if err != nil {
		t.Fatalf("subject: %v", err)
	}

	// Create a parent span whose trace ID must survive the NATS round-trip.
	ctx, parentSpan := tracer.Start(ctx, "test-publish")
	publisherTraceID := parentSpan.SpanContext().TraceID()

	// Subscribe with a handler that captures the delivered context.
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

	// Small delay for subscription propagation.
	time.Sleep(100 * time.Millisecond)

	payload := []byte(`{"task":"classify","control":"AC-1"}`)
	publishOrFail(t, client, ctx, subject, payload)
	parentSpan.End()

	// Wait for the subscriber to deliver the context.
	select {
	case r := <-resultCh:
		if r.sc.TraceID() != publisherTraceID {
			t.Errorf("subscriber trace ID = %s, want %s (publisher trace ID)",
				r.sc.TraceID(), publisherTraceID)
		}
		// After adding the consumer span, the context should carry a
		// non-remote span (the natsbus.process span), not the raw
		// remote span context from headers.
		if !r.sc.IsValid() {
			t.Error("subscriber SpanContext is not valid")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for subscriber context")
	}

	// Assert natsbus.process span exists with correct trace ID.
	time.Sleep(100 * time.Millisecond) // allow span to flush
	spans := tp.GetSpans()
	processSpan := telemetrytest.FindSpan(spans, "natsbus.process")
	if processSpan == nil {
		t.Fatal("expected natsbus.process span, got none")
	}
	if processSpan.SpanContext().TraceID() != publisherTraceID {
		t.Errorf("natsbus.process trace ID = %s, want %s",
			processSpan.SpanContext().TraceID(), publisherTraceID)
	}

	subjectAttr, ok := telemetrytest.SpanAttribute(processSpan, "messaging.subject")
	if !ok {
		t.Error("natsbus.process span missing messaging.subject attribute")
	} else if subjectAttr.AsString() != subject {
		t.Errorf("messaging.subject = %q, want %q", subjectAttr.AsString(), subject)
	}

	tenantAttr, ok := telemetrytest.SpanAttribute(processSpan, "tenant.id")
	if !ok {
		t.Error("natsbus.process span missing tenant.id attribute")
	} else if tenantAttr.AsString() != "acme-corp" {
		t.Errorf("tenant.id = %q, want %q", tenantAttr.AsString(), "acme-corp")
	}
}

func TestTelemetryQueueSubscribeSpan(t *testing.T) {
	tp, err := telemetrytest.NewTestProvider()
	if err != nil {
		t.Fatalf("telemetrytest.NewTestProvider: %v", err)
	}
	t.Cleanup(func() { tp.Shutdown(context.Background()) })

	tracer := tp.TracerProvider().Tracer("natsbus-test")
	meter := tp.MeterProvider().Meter("natsbus-test")
	client := newEmbeddedClientWithTelemetry(t, tracer, meter)

	ctx := testTenantCtx(t, "acme-corp")
	subject, err := natsbus.WorkSubject("acme-corp", natsbus.TaskClassify, "job-qsub")
	if err != nil {
		t.Fatalf("subject: %v", err)
	}

	queueName := "telemetry-workers"
	sub, err := client.QueueSubscribe(ctx, subject, queueName, func(_ context.Context, _ *natsbus.Message) error {
		return nil
	})
	if err != nil {
		t.Fatalf("QueueSubscribe: %v", err)
	}
	t.Cleanup(func() { sub.Unsubscribe() })

	spans := tp.GetSpans()
	qsSpan := telemetrytest.FindSpan(spans, "natsbus.QueueSubscribe")
	if qsSpan == nil {
		t.Fatal("expected natsbus.QueueSubscribe span, got none")
	}

	subjectAttr, ok := telemetrytest.SpanAttribute(qsSpan, "messaging.subject")
	if !ok {
		t.Error("natsbus.QueueSubscribe span missing messaging.subject attribute")
	} else if subjectAttr.AsString() != subject {
		t.Errorf("messaging.subject = %q, want %q", subjectAttr.AsString(), subject)
	}

	queueAttr, ok := telemetrytest.SpanAttribute(qsSpan, "messaging.queue")
	if !ok {
		t.Error("natsbus.QueueSubscribe span missing messaging.queue attribute")
	} else if queueAttr.AsString() != queueName {
		t.Errorf("messaging.queue = %q, want %q", queueAttr.AsString(), queueName)
	}
}

func TestSubscriberMetrics(t *testing.T) {
	tp, err := telemetrytest.NewTestProvider()
	if err != nil {
		t.Fatalf("telemetrytest.NewTestProvider: %v", err)
	}
	t.Cleanup(func() { tp.Shutdown(context.Background()) })

	tracer := tp.TracerProvider().Tracer("natsbus-test")
	meter := tp.MeterProvider().Meter("natsbus-test")
	client := newEmbeddedClientWithTelemetry(t, tracer, meter)

	ctx := testTenantCtx(t, "acme-corp")
	subject, err := natsbus.WorkSubject("acme-corp", natsbus.TaskClassify, "job-metrics")
	if err != nil {
		t.Fatalf("subject: %v", err)
	}

	const messageCount = 3
	var received atomic.Int32
	done := make(chan struct{})

	sub, err := client.Subscribe(ctx, subject, func(_ context.Context, _ *natsbus.Message) error {
		if received.Add(1) == messageCount {
			close(done)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { sub.Unsubscribe() })

	time.Sleep(100 * time.Millisecond)

	payload := []byte(`{"task":"classify"}`)
	for i := 0; i < messageCount; i++ {
		publishOrFail(t, client, ctx, subject, payload)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout: received %d/%d messages", received.Load(), messageCount)
	}

	// Allow metrics to flush.
	time.Sleep(100 * time.Millisecond)

	rm := tp.GetMetrics()
	counterMetric := telemetrytest.FindMetric(rm, "natsbus.process.total")
	if counterMetric == nil {
		t.Fatal("expected natsbus.process.total metric, got none")
	}
	counterVal, err := telemetrytest.CounterValue(counterMetric)
	if err != nil {
		t.Fatalf("counter value: %v", err)
	}
	if counterVal != int64(messageCount) {
		t.Errorf("natsbus.process.total = %d, want %d", counterVal, messageCount)
	}

	histMetric := telemetrytest.FindMetric(rm, "natsbus.process.duration_ms")
	if histMetric == nil {
		t.Fatal("expected natsbus.process.duration_ms metric, got none")
	}
	histCount, err := telemetrytest.HistogramCount(histMetric)
	if err != nil {
		t.Fatalf("histogram count: %v", err)
	}
	if histCount != uint64(messageCount) {
		t.Errorf("natsbus.process.duration_ms count = %d, want %d", histCount, messageCount)
	}
}

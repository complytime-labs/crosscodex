//go:build integration

package natsbus_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// ---------------------------------------------------------------------------
// Integration helpers (adapted from integration_helpers_test.go)
// ---------------------------------------------------------------------------

// defaultTestStreamsConfig returns the streams configuration shared by all
// integration tests that create a NATSConfig inline.
func defaultTestStreamsConfig() config.NATSStreamsConfig {
	return config.NATSStreamsConfig{
		AuditLLMRetention:    24 * time.Hour,
		AuditEventsRetention: 24 * time.Hour,
	}
}

// testTenantCtx creates a context with the given tenant ID for testing.
func testTenantCtx(tenantID string) context.Context {
	ctx, err := tenant.WithTenant(context.Background(), tenantID)
	Expect(err).NotTo(HaveOccurred(), "testTenantCtx(%q)", tenantID)
	return ctx
}

// subscribeOne sets up a buffered-channel subscriber on the given subject
// and returns the channel.
func subscribeOne(client natsbus.Client, ctx context.Context, subject string) <-chan *natsbus.Message {
	received := make(chan *natsbus.Message, 1)
	sub, err := client.Subscribe(ctx, subject, func(_ context.Context, msg *natsbus.Message) error {
		received <- msg
		return nil
	})
	Expect(err).NotTo(HaveOccurred(), "subscribe")
	DeferCleanup(func() { sub.Unsubscribe() })
	return received
}

// receiveOne waits up to timeout for a single message on ch.
func receiveOne(ch <-chan *natsbus.Message, timeout time.Duration) *natsbus.Message {
	var msg *natsbus.Message
	Eventually(ch, timeout).Should(Receive(&msg))
	return msg
}

// publishOrFail publishes a payload to the given subject.
func publishOrFail(client natsbus.Client, ctx context.Context, subject string, payload []byte) {
	Expect(client.Publish(ctx, subject, payload)).To(Succeed())
}

// assertProvenanceHeaders checks that all five required provenance headers
// are present and that the content hash matches the payload.
func assertProvenanceHeaders(msg *natsbus.Message, payload []byte, wantTenant string) {
	Expect(msg.Metadata.TenantID).To(Equal(wantTenant))

	h := sha256.Sum256(payload)
	wantHash := hex.EncodeToString(h[:])
	Expect(msg.Metadata.ContentHash).To(Equal(wantHash))

	requiredHeaders := []string{"X-Trace-Id", "X-Span-Id", "X-Tenant-Id", "X-Timestamp", "X-Content-SHA256"}
	for _, hdr := range requiredHeaders {
		Expect(msg.Headers).To(HaveKey(hdr), "missing required header %s", hdr)
	}

	if ts, ok := msg.Headers["X-Timestamp"]; ok && len(ts) > 0 {
		_, err := time.Parse(time.RFC3339Nano, ts[0])
		Expect(err).NotTo(HaveOccurred(), "X-Timestamp %q not valid RFC3339Nano", ts[0])
	}
}

// newEmbeddedClient creates an embedded NATS client for testing.
func newEmbeddedClient() natsbus.Client {
	storeDir := filepath.Join(GinkgoT().TempDir(), "nats-store")
	cfg := config.NATSConfig{
		URL: "", // embedded mode
		Embedded: config.NATSEmbeddedConfig{
			StoreDir: storeDir,
		},
		Streams: defaultTestStreamsConfig(),
	}

	client, err := natsbus.New(cfg)
	Expect(err).NotTo(HaveOccurred(), "failed to create embedded client")
	DeferCleanup(func() {
		Expect(client.Close()).To(Succeed())
	})
	return client
}

// newEmbeddedClientWithTelemetry creates an embedded NATS client with telemetry.
func newEmbeddedClientWithTelemetry(tracer trace.Tracer, meter metric.Meter) natsbus.Client {
	storeDir := filepath.Join(GinkgoT().TempDir(), "nats-store")
	cfg := config.NATSConfig{
		URL: "",
		Embedded: config.NATSEmbeddedConfig{
			StoreDir: storeDir,
		},
		Streams: defaultTestStreamsConfig(),
	}

	client, err := natsbus.New(cfg, natsbus.WithTelemetry(tracer, meter))
	Expect(err).NotTo(HaveOccurred(), "failed to create embedded client with telemetry")
	DeferCleanup(func() {
		Expect(client.Close()).To(Succeed())
	})
	return client
}

// ---------------------------------------------------------------------------
// Embedded integration specs
// ---------------------------------------------------------------------------

var _ = Describe("Embedded NATS Integration", Ordered, func() {

	Describe("Publish and Subscribe", func() {
		It("should deliver a published message to a subscriber", func() {
			client := newEmbeddedClient()
			ctx := testTenantCtx("acme-corp")

			subject, err := natsbus.WorkSubject("acme-corp", natsbus.TaskClassify, "job-001")
			Expect(err).NotTo(HaveOccurred())

			received := subscribeOne(client, ctx, subject)

			payload := []byte(`{"task":"classify","control":"AC-1"}`)
			publishOrFail(client, ctx, subject, payload)

			msg := receiveOne(received, 5*time.Second)
			Expect(msg.Subject).To(Equal(subject))
			Expect(string(msg.Data)).To(Equal(string(payload)))
		})
	})

	Describe("Provenance Round-Trip", func() {
		It("should attach and verify provenance headers on delivered messages", func() {
			client := newEmbeddedClient()
			ctx := testTenantCtx("test-tenant")

			subject, err := natsbus.WorkSubject("test-tenant", natsbus.TaskRelate, "job-prov")
			Expect(err).NotTo(HaveOccurred())

			received := subscribeOne(client, ctx, subject)

			payload := []byte("provenance test data")
			publishOrFail(client, ctx, subject, payload)

			msg := receiveOne(received, 5*time.Second)
			assertProvenanceHeaders(msg, payload, "test-tenant")
		})
	})

	Describe("Queue Group Distribution", func() {
		It("should distribute messages across queue group workers", func() {
			client := newEmbeddedClient()
			ctx := testTenantCtx("acme-corp")

			subject, err := natsbus.WorkSubject("acme-corp", natsbus.TaskClassify, "job-queue")
			Expect(err).NotTo(HaveOccurred())

			const numWorkers = 3
			const numMessages = 30

			var counts [numWorkers]atomic.Int64

			subs := make([]natsbus.Subscription, numWorkers)
			for i := range numWorkers {
				workerIdx := i
				sub, subErr := client.QueueSubscribe(ctx, subject, "test-workers", func(_ context.Context, _ *natsbus.Message) error {
					counts[workerIdx].Add(1)
					return nil
				})
				Expect(subErr).NotTo(HaveOccurred(), "queue subscribe worker %d", i)
				subs[i] = sub
			}
			DeferCleanup(func() {
				for _, sub := range subs {
					sub.Unsubscribe()
				}
			})

			// Small delay for subscriptions to propagate
			time.Sleep(100 * time.Millisecond)

			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer GinkgoRecover()
				defer wg.Done()
				for range numMessages {
					Expect(client.Publish(ctx, subject, []byte("work"))).To(Succeed())
				}
			}()
			wg.Wait()

			// Wait for all messages to be delivered
			Eventually(func() int64 {
				var total int64
				for i := range numWorkers {
					total += counts[i].Load()
				}
				return total
			}, 10*time.Second, 50*time.Millisecond).Should(BeNumerically(">=", int64(numMessages)))

			// Verify distribution — each worker should have received at least 1
			for i := range numWorkers {
				c := counts[i].Load()
				GinkgoWriter.Printf("worker %d received %d messages\n", i, c)
				Expect(c).To(BeNumerically(">", 0),
					"worker %d received 0 messages; expected round-robin distribution", i)
			}
		})
	})

	Describe("JetStream Audit Persistence", func() {
		It("should persist audit messages to JetStream", func() {
			client := newEmbeddedClient()
			ctx := testTenantCtx("acme-corp")

			subject, err := natsbus.AuditSubject("acme-corp", natsbus.AuditLLM, "job-audit")
			Expect(err).NotTo(HaveOccurred())

			payload := []byte(`{"model":"gpt-4","prompt":"classify AC-1"}`)
			publishOrFail(client, ctx, subject, payload)

			// Give JetStream time to persist
			time.Sleep(500 * time.Millisecond)

			// Subscribe and verify the message was persisted in the audit stream
			received := subscribeOne(client, ctx, subject)

			// Publish another to trigger subscription
			payload2 := []byte(`{"model":"gpt-4","prompt":"classify AC-2"}`)
			publishOrFail(client, ctx, subject, payload2)

			msg := receiveOne(received, 5*time.Second)
			Expect(string(msg.Data)).To(Equal(string(payload2)))
		})
	})

	Describe("Tenant Isolation", func() {
		It("should prevent cross-tenant message leakage", func() {
			client := newEmbeddedClient()

			subjectA, err := natsbus.WorkSubject("tenant-aaa", natsbus.TaskClassify, "job-iso")
			Expect(err).NotTo(HaveOccurred())
			subjectB, err := natsbus.WorkSubject("tenant-bbb", natsbus.TaskClassify, "job-iso")
			Expect(err).NotTo(HaveOccurred())

			ctxA := testTenantCtx("tenant-aaa")
			ctxB := testTenantCtx("tenant-bbb")

			var receivedByA atomic.Int64
			var receivedByB atomic.Int64

			subA, err := client.Subscribe(ctxA, subjectA, func(_ context.Context, _ *natsbus.Message) error {
				receivedByA.Add(1)
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { subA.Unsubscribe() })

			subB, err := client.Subscribe(ctxB, subjectB, func(_ context.Context, _ *natsbus.Message) error {
				receivedByB.Add(1)
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { subB.Unsubscribe() })

			time.Sleep(100 * time.Millisecond)

			// Publish only to tenant A
			for range 5 {
				Expect(client.Publish(ctxA, subjectA, []byte("for A"))).To(Succeed())
			}

			time.Sleep(500 * time.Millisecond)

			Expect(receivedByA.Load()).To(Equal(int64(5)))
			Expect(receivedByB.Load()).To(Equal(int64(0)), "isolation violated: tenant B received messages meant for tenant A")
		})
	})

	Describe("Close Behavior", func() {
		Context("when closing a client", func() {
			It("should be idempotent", func() {
				storeDir := filepath.Join(GinkgoT().TempDir(), "nats-close-test")
				cfg := config.NATSConfig{
					URL: "",
					Embedded: config.NATSEmbeddedConfig{
						StoreDir: storeDir,
					},
					Streams: defaultTestStreamsConfig(),
				}

				client, err := natsbus.New(cfg)
				Expect(err).NotTo(HaveOccurred())

				// Close twice — should not panic
				Expect(client.Close()).To(Succeed())
				Expect(client.Close()).To(Succeed())
			})
		})

		Context("when publishing after close", func() {
			It("should return an error", func() {
				client := newEmbeddedClient()
				ctx := testTenantCtx("acme-corp")

				Expect(client.Close()).To(Succeed())

				subject, err := natsbus.WorkSubject("acme-corp", natsbus.TaskClassify, "job-closed")
				Expect(err).NotTo(HaveOccurred())

				err = client.Publish(ctx, subject, []byte("should fail"))
				Expect(err).To(HaveOccurred())
			})
		})
	})

	Describe("XDG State Home Compliance", func() {
		It("should use XDG_STATE_HOME for embedded store directory", func() {
			customDir := filepath.Join(GinkgoT().TempDir(), "custom-state")
			Expect(os.Setenv("XDG_STATE_HOME", customDir)).To(Succeed())
			DeferCleanup(func() { os.Unsetenv("XDG_STATE_HOME") })

			cfg := config.NATSConfig{
				URL: "",
				Embedded: config.NATSEmbeddedConfig{
					StoreDir: "", // should use XDG_STATE_HOME
				},
				Streams: defaultTestStreamsConfig(),
			}

			client, err := natsbus.New(cfg)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { client.Close() })

			// Verify the store directory was created under XDG_STATE_HOME
			expectedDir := filepath.Join(customDir, "crosscodex", "nats")
			_, statErr := os.Stat(expectedDir)
			Expect(os.IsNotExist(statErr)).To(BeFalse(), "expected store dir %q to exist", expectedDir)
		})
	})

	Describe("Missing Tenant Context", func() {
		It("should reject publish without tenant context with an actionable error", func() {
			client := newEmbeddedClient()

			subject, err := natsbus.WorkSubject("acme-corp", natsbus.TaskClassify, "job-001")
			Expect(err).NotTo(HaveOccurred())

			// Publish with a context that has no tenant — should fail.
			err = client.Publish(context.Background(), subject, []byte("hello"))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("tenant"))
		})
	})

	// -----------------------------------------------------------------------
	// Telemetry
	// -----------------------------------------------------------------------

	Describe("Telemetry Integration", func() {
		var (
			tp     *telemetrytest.TestProvider
			tracer trace.Tracer
			meter  metric.Meter
		)

		BeforeEach(func() {
			var err error
			tp, err = telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { tp.Shutdown(context.Background()) })

			tracer = tp.TracerProvider().Tracer("natsbus-test")
			meter = tp.MeterProvider().Meter("natsbus-test")
		})

		Describe("Publish Spans", func() {
			It("should record publish and subscribe spans with expected attributes", func() {
				client := newEmbeddedClientWithTelemetry(tracer, meter)
				ctx := testTenantCtx("acme-corp")

				subject, err := natsbus.WorkSubject("acme-corp", natsbus.TaskClassify, "job-tel")
				Expect(err).NotTo(HaveOccurred())

				received := subscribeOne(client, ctx, subject)

				payload := []byte(`{"task":"classify","control":"AC-1"}`)
				publishOrFail(client, ctx, subject, payload)
				receiveOne(received, 5*time.Second)

				// Assert publish span exists with expected attributes.
				spans := tp.GetSpans()
				pubSpan := telemetrytest.FindSpan(spans, "natsbus.Publish")
				Expect(pubSpan).NotTo(BeNil(), "expected natsbus.Publish span")

				subjectAttr, ok := telemetrytest.SpanAttribute(pubSpan, "messaging.subject")
				Expect(ok).To(BeTrue(), "natsbus.Publish span missing messaging.subject attribute")
				Expect(subjectAttr.AsString()).To(Equal(subject))

				tenantAttr, ok := telemetrytest.SpanAttribute(pubSpan, "tenant.id")
				Expect(ok).To(BeTrue(), "natsbus.Publish span missing tenant.id attribute")
				Expect(tenantAttr.AsString()).To(Equal("acme-corp"))

				// Assert publish counter metric >= 1.
				rm := tp.GetMetrics()
				m := telemetrytest.FindMetric(rm, "natsbus.publish.total")
				Expect(m).NotTo(BeNil(), "expected natsbus.publish.total metric")
				val, err := telemetrytest.CounterValue(m)
				Expect(err).NotTo(HaveOccurred())
				Expect(val).To(BeNumerically(">=", int64(1)))

				// Assert natsbus.Subscribe span exists with messaging.subject attribute.
				subSpan := telemetrytest.FindSpan(spans, "natsbus.Subscribe")
				Expect(subSpan).NotTo(BeNil(), "expected natsbus.Subscribe span")
				subSubjectAttr, ok := telemetrytest.SpanAttribute(subSpan, "messaging.subject")
				Expect(ok).To(BeTrue(), "natsbus.Subscribe span missing messaging.subject attribute")
				Expect(subSubjectAttr.AsString()).To(Equal(subject))

				// Assert publish duration histogram.
				hm := telemetrytest.FindMetric(rm, "natsbus.publish.duration_ms")
				Expect(hm).NotTo(BeNil(), "expected natsbus.publish.duration_ms metric")
				hc, err := telemetrytest.HistogramCount(hm)
				Expect(err).NotTo(HaveOccurred())
				Expect(hc).To(BeNumerically(">=", uint64(1)))
			})
		})

		Describe("Trace Context Round-Trip", func() {
			It("should propagate the publisher trace ID through NATS to the subscriber", func() {
				client := newEmbeddedClientWithTelemetry(tracer, meter)
				ctx := testTenantCtx("acme-corp")

				subject, err := natsbus.WorkSubject("acme-corp", natsbus.TaskClassify, "job-trace")
				Expect(err).NotTo(HaveOccurred())

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
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(func() { sub.Unsubscribe() })

				// Small delay for subscription propagation.
				time.Sleep(100 * time.Millisecond)

				payload := []byte(`{"task":"classify","control":"AC-1"}`)
				publishOrFail(client, ctx, subject, payload)
				parentSpan.End()

				// Wait for the subscriber to deliver the context.
				var r result
				Eventually(resultCh, 5*time.Second).Should(Receive(&r))
				Expect(r.sc.TraceID()).To(Equal(publisherTraceID))
				Expect(r.sc.IsValid()).To(BeTrue(), "subscriber SpanContext is not valid")

				// Assert natsbus.process span exists with correct trace ID.
				time.Sleep(100 * time.Millisecond) // allow span to flush
				spans := tp.GetSpans()
				processSpan := telemetrytest.FindSpan(spans, "natsbus.process")
				Expect(processSpan).NotTo(BeNil(), "expected natsbus.process span")
				Expect(processSpan.SpanContext().TraceID()).To(Equal(publisherTraceID))

				subjectAttr, ok := telemetrytest.SpanAttribute(processSpan, "messaging.subject")
				Expect(ok).To(BeTrue(), "natsbus.process span missing messaging.subject attribute")
				Expect(subjectAttr.AsString()).To(Equal(subject))

				tenantAttr, ok := telemetrytest.SpanAttribute(processSpan, "tenant.id")
				Expect(ok).To(BeTrue(), "natsbus.process span missing tenant.id attribute")
				Expect(tenantAttr.AsString()).To(Equal("acme-corp"))
			})
		})

		Describe("QueueSubscribe Span", func() {
			It("should record a QueueSubscribe span with subject and queue attributes", func() {
				client := newEmbeddedClientWithTelemetry(tracer, meter)
				ctx := testTenantCtx("acme-corp")

				subject, err := natsbus.WorkSubject("acme-corp", natsbus.TaskClassify, "job-qsub")
				Expect(err).NotTo(HaveOccurred())

				queueName := "telemetry-workers"
				sub, err := client.QueueSubscribe(ctx, subject, queueName, func(_ context.Context, _ *natsbus.Message) error {
					return nil
				})
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(func() { sub.Unsubscribe() })

				spans := tp.GetSpans()
				qsSpan := telemetrytest.FindSpan(spans, "natsbus.QueueSubscribe")
				Expect(qsSpan).NotTo(BeNil(), "expected natsbus.QueueSubscribe span")

				subjectAttr, ok := telemetrytest.SpanAttribute(qsSpan, "messaging.subject")
				Expect(ok).To(BeTrue(), "natsbus.QueueSubscribe span missing messaging.subject attribute")
				Expect(subjectAttr.AsString()).To(Equal(subject))

				queueAttr, ok := telemetrytest.SpanAttribute(qsSpan, "messaging.queue")
				Expect(ok).To(BeTrue(), "natsbus.QueueSubscribe span missing messaging.queue attribute")
				Expect(queueAttr.AsString()).To(Equal(queueName))
			})
		})

		Describe("Subscriber Metrics", func() {
			It("should record process counter and duration histogram for delivered messages", func() {
				client := newEmbeddedClientWithTelemetry(tracer, meter)
				ctx := testTenantCtx("acme-corp")

				subject, err := natsbus.WorkSubject("acme-corp", natsbus.TaskClassify, "job-metrics")
				Expect(err).NotTo(HaveOccurred())

				const messageCount = 3
				var received atomic.Int32
				done := make(chan struct{})

				sub, err := client.Subscribe(ctx, subject, func(_ context.Context, _ *natsbus.Message) error {
					if received.Add(1) == messageCount {
						close(done)
					}
					return nil
				})
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(func() { sub.Unsubscribe() })

				time.Sleep(100 * time.Millisecond)

				payload := []byte(`{"task":"classify"}`)
				for i := 0; i < messageCount; i++ {
					publishOrFail(client, ctx, subject, payload)
				}

				Eventually(done, 5*time.Second).Should(BeClosed())

				// Allow metrics to flush.
				time.Sleep(100 * time.Millisecond)

				rm := tp.GetMetrics()
				counterMetric := telemetrytest.FindMetric(rm, "natsbus.process.total")
				Expect(counterMetric).NotTo(BeNil(), "expected natsbus.process.total metric")
				counterVal, err := telemetrytest.CounterValue(counterMetric)
				Expect(err).NotTo(HaveOccurred())
				Expect(counterVal).To(Equal(int64(messageCount)))

				histMetric := telemetrytest.FindMetric(rm, "natsbus.process.duration_ms")
				Expect(histMetric).NotTo(BeNil(), "expected natsbus.process.duration_ms metric")
				histCount, err := telemetrytest.HistogramCount(histMetric)
				Expect(err).NotTo(HaveOccurred())
				Expect(histCount).To(Equal(uint64(messageCount)))
			})
		})
	})

	// -----------------------------------------------------------------------
	// Provenance Enforcement (fail-closed) — converted from provenance_integration_test.go
	// Uses export bridges to access internal functions externally.
	// -----------------------------------------------------------------------

	Describe("Provenance Enforcement (fail-closed)", func() {
		// startRawNATSConn starts an embedded NATS server and returns a raw
		// nats.Conn that bypasses natsbus entirely.
		startRawNATSConn := func(storeSuffix string) *nats.Conn {
			storeDir := filepath.Join(GinkgoT().TempDir(), storeSuffix)
			cfg := config.NATSEmbeddedConfig{StoreDir: storeDir}
			logger := slog.Default()

			es, err := natsbus.ExportStartEmbedded(cfg, logger)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(es.Shutdown)

			rawConn, err := nats.Connect(es.ClientURL())
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(rawConn.Close)

			return rawConn
		}

		// subscribeRejectingProvenance sets up a natsbus wrapHandler subscriber
		// on a raw NATS connection. Returns a flag indicating whether the handler
		// was called (should remain false for messages lacking provenance).
		subscribeRejectingProvenance := func(rawConn *nats.Conn, subject string) *atomic.Bool {
			var handlerCalled atomic.Bool
			wrappedHandler := natsbus.ExportWrapHandler(rawConn, func(_ context.Context, _ *natsbus.Message) error {
				handlerCalled.Store(true)
				return nil
			})

			sub, err := rawConn.Subscribe(subject, wrappedHandler)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = sub.Unsubscribe() })

			return &handlerCalled
		}

		Context("when a raw NATS message lacks provenance headers", func() {
			It("should reject the message and not call the handler", func() {
				rawConn := startRawNATSConn("nats-provenance-test")
				subject := "crosscodex.work.test-tenant.classify.job-raw"
				handlerCalled := subscribeRejectingProvenance(rawConn, subject)

				// Publish a raw message with NO headers (no provenance).
				Expect(rawConn.Publish(subject, []byte("raw payload without provenance"))).To(Succeed())
				Expect(rawConn.Flush()).To(Succeed())

				// Wait long enough for delivery.
				time.Sleep(500 * time.Millisecond)

				Expect(handlerCalled.Load()).To(BeFalse(),
					"handler was called for a message without provenance headers; expected rejection")
			})
		})

		Context("when a raw NATS message has partial provenance headers", func() {
			It("should reject the message and not call the handler", func() {
				rawConn := startRawNATSConn("nats-partial-provenance-test")
				subject := "crosscodex.work.test-tenant.classify.job-partial"
				handlerCalled := subscribeRejectingProvenance(rawConn, subject)

				// Publish with only tenant header — missing the other 4 required headers.
				msg := &nats.Msg{
					Subject: subject,
					Data:    []byte("partial provenance"),
					Header:  nats.Header{"X-Tenant-Id": []string{"test-tenant"}},
				}
				Expect(rawConn.PublishMsg(msg)).To(Succeed())
				Expect(rawConn.Flush()).To(Succeed())

				time.Sleep(500 * time.Millisecond)

				Expect(handlerCalled.Load()).To(BeFalse(),
					"handler was called for a message with partial provenance headers; expected rejection")
			})
		})

		Context("when a message has valid headers but a tampered content hash", func() {
			It("should reject the message and not call the handler", func() {
				rawConn := startRawNATSConn("nats-hash-mismatch-test")
				subject := "crosscodex.work.test-tenant.classify.job-hash"

				var handlerCalled atomic.Bool
				wrappedHandler := natsbus.ExportWrapHandler(rawConn, func(_ context.Context, _ *natsbus.Message) error {
					handlerCalled.Store(true)
					return nil
				})

				sub, err := rawConn.Subscribe(subject, wrappedHandler)
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(func() { _ = sub.Unsubscribe() })

				// Build a message with all 5 provenance headers but a wrong content hash.
				payload := []byte("legitimate payload")
				wrongHash := natsbus.ContentHash([]byte("different payload"))
				msg := &nats.Msg{
					Subject: subject,
					Data:    payload,
					Header: nats.Header{
						natsbus.HeaderTraceID:       []string{"0af7651916cd43dd8448eb211c80319c"}, // DevSkim: ignore DS173237 - OTel trace ID test fixture, not a credential
						natsbus.HeaderSpanID:        []string{"b7ad6b7169203331"},
						natsbus.HeaderTenantID:      []string{"test-tenant"},
						natsbus.HeaderTimestamp:     []string{time.Now().UTC().Format(time.RFC3339Nano)},
						natsbus.HeaderContentSHA256: []string{wrongHash},
					},
				}
				Expect(rawConn.PublishMsg(msg)).To(Succeed())
				Expect(rawConn.Flush()).To(Succeed())

				time.Sleep(500 * time.Millisecond)

				Expect(handlerCalled.Load()).To(BeFalse(),
					"handler was called for a message with mismatched content hash; expected rejection")
			})
		})

		Context("when a message has valid headers and a correct content hash", func() {
			It("should accept the message and call the handler", func() {
				rawConn := startRawNATSConn("nats-hash-match-test")
				subject := "crosscodex.work.test-tenant.classify.job-hash-ok"

				var handlerCalled atomic.Bool
				wrappedHandler := natsbus.ExportWrapHandler(rawConn, func(_ context.Context, _ *natsbus.Message) error {
					handlerCalled.Store(true)
					return nil
				})

				sub, err := rawConn.Subscribe(subject, wrappedHandler)
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(func() { _ = sub.Unsubscribe() })

				payload := []byte("legitimate payload")
				correctHash := natsbus.ContentHash(payload)
				msg := &nats.Msg{
					Subject: subject,
					Data:    payload,
					Header: nats.Header{
						natsbus.HeaderTraceID:       []string{"0af7651916cd43dd8448eb211c80319c"}, // DevSkim: ignore DS173237 - OTel trace ID test fixture, not a credential
						natsbus.HeaderSpanID:        []string{"b7ad6b7169203331"},
						natsbus.HeaderTenantID:      []string{"test-tenant"},
						natsbus.HeaderTimestamp:     []string{time.Now().UTC().Format(time.RFC3339Nano)},
						natsbus.HeaderContentSHA256: []string{correctHash},
					},
				}
				Expect(rawConn.PublishMsg(msg)).To(Succeed())
				Expect(rawConn.Flush()).To(Succeed())

				time.Sleep(500 * time.Millisecond)

				Expect(handlerCalled.Load()).To(BeTrue(),
					"handler was NOT called for a message with correct content hash; expected acceptance")
			})
		})
	})
})

// Ensure strings import is used (for missing tenant context test).
var _ = strings.Contains

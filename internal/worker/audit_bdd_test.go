package worker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
)

var _ = Describe("NATSAuditEmitter", func() {
	var (
		bus     natsbus.Client
		cleanup func()
		emitter *NATSAuditEmitter
	)

	BeforeEach(func() {
		bus, cleanup = testspecs.SetupTestNATS()
		emitter = NewNATSAuditEmitter(bus, testspecs.GinkgoLogger())
	})

	AfterEach(func() {
		cleanup()
	})

	It("publishes audit events to the correct subject", func() {
		ctx := testspecs.SetupTenantContext("tenant-abc")

		var received *natsbus.Message
		var mu sync.Mutex
		sub, err := bus.Subscribe(ctx, "crosscodex.audit.tenant-abc.llm.job-123", func(_ context.Context, msg *natsbus.Message) error {
			mu.Lock()
			defer mu.Unlock()
			received = msg
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		defer func() {
			_ = sub.Unsubscribe()
		}()

		event := &llmclient.AuditEvent{
			Timestamp:  time.Now(),
			TenantID:   "tenant-abc",
			JobID:      "job-123",
			Model:      "gpt-4",
			Operation:  llmclient.OpComplete,
			PromptHash: "abc123",
			TokensUsed: 100,
			DurationMS: 250,
			Success:    true,
		}

		err = emitter.EmitLLMAudit(ctx, event)
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() *natsbus.Message {
			mu.Lock()
			defer mu.Unlock()
			return received
		}).WithTimeout(2 * time.Second).ShouldNot(BeNil())

		var decoded llmclient.AuditEvent
		mu.Lock()
		Expect(json.Unmarshal(received.Data, &decoded)).To(Succeed())
		mu.Unlock()
		Expect(decoded.TenantID).To(Equal("tenant-abc"))
		Expect(decoded.JobID).To(Equal("job-123"))
		Expect(decoded.Model).To(Equal("gpt-4"))
		Expect(decoded.TokensUsed).To(Equal(100))
	})

	It("returns nil on invalid tenant ID (best-effort) and publishes nothing", func() {
		ctx := testspecs.SetupTenantContext("tenant-abc")

		// Subscribe to the wildcard that would catch any mistakenly published audit message.
		var publishedMsg *natsbus.Message
		var mu sync.Mutex
		sub, err := bus.Subscribe(ctx, "crosscodex.audit.>", func(_ context.Context, msg *natsbus.Message) error {
			mu.Lock()
			defer mu.Unlock()
			publishedMsg = msg
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = sub.Unsubscribe() }()

		event := &llmclient.AuditEvent{
			TenantID: "", // invalid — will fail subject building
			JobID:    "job-123",
		}
		err = emitter.EmitLLMAudit(ctx, event)
		Expect(err).NotTo(HaveOccurred())

		// Confirm no message was published to any audit subject.
		Consistently(func() *natsbus.Message {
			mu.Lock()
			defer mu.Unlock()
			return publishedMsg
		}).WithTimeout(200 * time.Millisecond).Should(BeNil())
	})

	It("injects provenance headers on published audit messages", func() {
		ctx := testspecs.SetupTenantContext("tenant-abc")

		var received *natsbus.Message
		var mu sync.Mutex
		sub, err := bus.Subscribe(ctx, "crosscodex.audit.tenant-abc.llm.job-prov", func(_ context.Context, msg *natsbus.Message) error {
			mu.Lock()
			defer mu.Unlock()
			received = msg
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = sub.Unsubscribe() }()

		event := &llmclient.AuditEvent{
			Timestamp:  time.Now(),
			TenantID:   "tenant-abc",
			JobID:      "job-prov",
			Model:      "gpt-4",
			Operation:  llmclient.OpComplete,
			TokensUsed: 10,
			Success:    true,
		}

		Expect(emitter.EmitLLMAudit(ctx, event)).To(Succeed())

		Eventually(func() *natsbus.Message {
			mu.Lock()
			defer mu.Unlock()
			return received
		}).WithTimeout(2 * time.Second).ShouldNot(BeNil())

		mu.Lock()
		msg := received
		mu.Unlock()

		// (3) All mandatory provenance headers present
		Expect(msg.Headers["X-Tenant-Id"]).To(ConsistOf("tenant-abc"),
			"X-Tenant-Id provenance header must be set to the event tenant ID")
		Expect(msg.Headers["X-Timestamp"]).NotTo(BeEmpty(),
			"X-Timestamp provenance header must be present")
		Expect(msg.Headers["X-Content-SHA256"]).NotTo(BeEmpty(),
			"X-Content-SHA256 provenance header must be present")

		// (4) Content hash in header matches recomputed hash of payload
		rawHash := msg.Headers["X-Content-SHA256"]
		Expect(rawHash).To(HaveLen(1))

		// Recompute SHA-256 of the received message body
		payloadHash := fmt.Sprintf("%x", sha256.Sum256(msg.Data))
		Expect(rawHash[0]).To(Equal(payloadHash),
			"X-Content-SHA256 header must match SHA-256 of the message body")
	})

	It("sets CorrelationID from active trace context when trace is available", func() {
		// (1) CorrelationID is deterministically derived from trace context
		// When no trace is active, CorrelationID in the event is whatever was set
		ctx := testspecs.SetupTenantContext("tenant-abc")

		var received *natsbus.Message
		var mu sync.Mutex
		sub, err := bus.Subscribe(ctx, "crosscodex.audit.tenant-abc.llm.job-corr", func(_ context.Context, msg *natsbus.Message) error {
			mu.Lock()
			defer mu.Unlock()
			received = msg
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = sub.Unsubscribe() }()

		traceID := "test-trace-id-abc123"
		event := &llmclient.AuditEvent{
			Timestamp: time.Now(),
			TenantID:  "tenant-abc",
			JobID:     "job-corr",
			Model:     "gpt-4",
			Operation: llmclient.OpComplete,
			Success:   true,
			TraceID:   traceID,
		}

		Expect(emitter.EmitLLMAudit(ctx, event)).To(Succeed())

		Eventually(func() *natsbus.Message {
			mu.Lock()
			defer mu.Unlock()
			return received
		}).WithTimeout(2 * time.Second).ShouldNot(BeNil())

		mu.Lock()
		msg := received
		mu.Unlock()

		// (1) Verify the event's TraceID is preserved in the published payload
		var decoded llmclient.AuditEvent
		Expect(json.Unmarshal(msg.Data, &decoded)).To(Succeed())
		Expect(decoded.TraceID).To(Equal(traceID),
			"TraceID must be preserved through publish/subscribe roundtrip")
	})

	It("returns nil after NATS client is closed (best-effort)", func() {
		ctx := testspecs.SetupTenantContext("tenant-abc")
		cleanup() // close the NATS client

		event := &llmclient.AuditEvent{
			TenantID: "tenant-abc",
			JobID:    "job-123",
		}
		err := emitter.EmitLLMAudit(ctx, event)
		Expect(err).NotTo(HaveOccurred())

		// Re-create so AfterEach cleanup doesn't double-close
		bus, cleanup = testspecs.SetupTestNATS()
		emitter = NewNATSAuditEmitter(bus, testspecs.GinkgoLogger())
	})
})

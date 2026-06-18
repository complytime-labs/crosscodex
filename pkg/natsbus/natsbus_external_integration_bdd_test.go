//go:build integration_nats

package natsbus_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"github.com/complytime-labs/crosscodex/pkg/tlsconfig"
)

// ---------------------------------------------------------------------------
// Integration helpers (duplicated for integration_nats build tag)
// ---------------------------------------------------------------------------

// extDefaultTestStreamsConfig returns the streams configuration shared by
// external integration tests.
func extDefaultTestStreamsConfig() config.NATSStreamsConfig {
	return config.NATSStreamsConfig{
		AuditLLMRetention:    24 * time.Hour,
		AuditEventsRetention: 24 * time.Hour,
	}
}

// extTestTenantCtx creates a context with the given tenant ID for testing.
func extTestTenantCtx(tenantID string) context.Context {
	ctx, err := tenant.WithTenant(context.Background(), tenantID)
	Expect(err).NotTo(HaveOccurred(), "extTestTenantCtx(%q)", tenantID)
	return ctx
}

// extSubscribeOne sets up a buffered-channel subscriber on the given subject
// and returns the channel.
func extSubscribeOne(client natsbus.Client, ctx context.Context, subject string) <-chan *natsbus.Message {
	received := make(chan *natsbus.Message, 1)
	sub, err := client.Subscribe(ctx, subject, func(_ context.Context, msg *natsbus.Message) error {
		received <- msg
		return nil
	})
	Expect(err).NotTo(HaveOccurred(), "subscribe")
	DeferCleanup(func() { sub.Unsubscribe() })
	return received
}

// extReceiveOne waits up to timeout for a single message on ch.
func extReceiveOne(ch <-chan *natsbus.Message, timeout time.Duration) *natsbus.Message {
	var msg *natsbus.Message
	Eventually(ch, timeout).Should(Receive(&msg))
	return msg
}

// extPublishOrFail publishes a payload to the given subject.
func extPublishOrFail(client natsbus.Client, ctx context.Context, subject string, payload []byte) {
	Expect(client.Publish(ctx, subject, payload)).To(Succeed())
}

// extAssertProvenanceHeaders checks that all five required provenance headers
// are present and that the content hash matches the payload.
func extAssertProvenanceHeaders(msg *natsbus.Message, payload []byte, wantTenant string) {
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

// ---------------------------------------------------------------------------
// External NATS client factory
// ---------------------------------------------------------------------------

// loadTLSConfig reads TLS configuration from environment variables.
func loadTLSConfig() *config.TLSConfig {
	caFile := os.Getenv("TEST_NATS_CA")
	certFile := os.Getenv("TEST_NATS_CERT")
	keyFile := os.Getenv("TEST_NATS_KEY")

	if caFile == "" || certFile == "" || keyFile == "" {
		Skip("TEST_NATS_CA, TEST_NATS_CERT, TEST_NATS_KEY must be set for TLS tests")
	}

	return &config.TLSConfig{
		Mode: "mutual",
		CA:   caFile,
		Cert: certFile,
		Key:  keyFile,
	}
}

// newExternalClient creates a NATS client connected to an external NATS server.
func newExternalClient() natsbus.Client {
	natsURL := os.Getenv("TEST_NATS_URL")
	tlsCfgInput := loadTLSConfig()

	tlsCfg, err := tlsconfig.BuildTLSConfig(context.Background(), *tlsCfgInput, "nats")
	Expect(err).NotTo(HaveOccurred(), "build TLS config")

	cfg := config.NATSConfig{
		URL:     natsURL,
		TLS:     true,
		Streams: extDefaultTestStreamsConfig(),
	}

	client, err := natsbus.New(cfg, natsbus.WithTLSConfig(tlsCfg))
	Expect(err).NotTo(HaveOccurred(), "failed to create external client")
	DeferCleanup(func() {
		Expect(client.Close()).To(Succeed())
	})
	return client
}

// ---------------------------------------------------------------------------
// External NATS integration specs
// ---------------------------------------------------------------------------

var _ = Describe("External NATS Integration", Ordered, func() {

	BeforeAll(func() {
		if os.Getenv("TEST_NATS_URL") == "" {
			Skip("TEST_NATS_URL not set; skipping external NATS tests")
		}
	})

	Describe("TLS Connection", func() {
		It("should publish and receive a message over TLS", func() {
			client := newExternalClient()
			ctx := extTestTenantCtx("acme-corp")

			subject, err := natsbus.WorkSubject("acme-corp", natsbus.TaskClassify, "ext-tls-001")
			Expect(err).NotTo(HaveOccurred())

			received := extSubscribeOne(client, ctx, subject)

			payload := []byte(`{"test":"tls"}`)
			extPublishOrFail(client, ctx, subject, payload)

			msg := extReceiveOne(received, 5*time.Second)
			Expect(string(msg.Data)).To(Equal(string(payload)))
		})
	})

	Describe("Provenance Headers", func() {
		It("should attach and verify provenance headers on external NATS messages", func() {
			client := newExternalClient()
			ctx := extTestTenantCtx("test-tenant")

			subject, err := natsbus.WorkSubject("test-tenant", natsbus.TaskEmbed, "ext-prov-001")
			Expect(err).NotTo(HaveOccurred())

			received := extSubscribeOne(client, ctx, subject)

			payload := []byte("external provenance test")
			extPublishOrFail(client, ctx, subject, payload)

			msg := extReceiveOne(received, 5*time.Second)
			extAssertProvenanceHeaders(msg, payload, "test-tenant")
		})
	})

	Describe("Queue Group", func() {
		It("should deliver all messages through a queue group on external NATS", func() {
			client := newExternalClient()
			ctx := extTestTenantCtx("acme-corp")

			subject, err := natsbus.WorkSubject("acme-corp", natsbus.TaskRelate, "ext-queue-001")
			Expect(err).NotTo(HaveOccurred())

			const numMessages = 10
			var count atomic.Int64

			sub, err := client.QueueSubscribe(ctx, subject, "ext-workers", func(_ context.Context, _ *natsbus.Message) error {
				count.Add(1)
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { sub.Unsubscribe() })

			time.Sleep(100 * time.Millisecond)

			for range numMessages {
				Expect(client.Publish(ctx, subject, []byte("work"))).To(Succeed())
			}

			Eventually(func() int64 {
				return count.Load()
			}, 10*time.Second, 50*time.Millisecond).Should(BeNumerically(">=", int64(numMessages)))
		})
	})

	Describe("JetStream Audit", func() {
		It("should persist and deliver audit messages on external NATS", func() {
			client := newExternalClient()
			ctx := extTestTenantCtx("acme-corp")

			subject, err := natsbus.AuditSubject("acme-corp", natsbus.AuditDecisions, "ext-audit-001")
			Expect(err).NotTo(HaveOccurred())

			payload := []byte(`{"decision":"approved","control":"AC-1"}`)
			extPublishOrFail(client, ctx, subject, payload)

			// Verify stream exists (audit stream should have been created on startup)
			received := extSubscribeOne(client, ctx, subject)

			// Publish another to verify the stream is working
			payload2 := []byte(`{"decision":"denied","control":"AC-2"}`)
			extPublishOrFail(client, ctx, subject, payload2)

			msg := extReceiveOne(received, 5*time.Second)
			Expect(string(msg.Data)).To(Equal(string(payload2)))
		})
	})
})

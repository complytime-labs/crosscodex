//go:build integration_nats

package natsbus_test

import (
	"context"
	"os"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/tlsconfig"
)

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
		Streams: defaultTestStreamsConfig(),
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

			payload := []byte(`{"test":"tls"}`)
			_, msg := publishAndReceive(client, "acme-corp", natsbus.TaskClassify, "ext-tls-001", payload)

			Expect(string(msg.Data)).To(Equal(string(payload)))
		})
	})

	Describe("Provenance Headers", func() {
		It("should attach and verify provenance headers on external NATS messages", func() {
			expectProvenanceRoundTrip(newExternalClient(), "test-tenant", natsbus.TaskEmbed, "ext-prov-001", []byte("external provenance test"))
		})
	})

	Describe("Queue Group", func() {
		It("should deliver all messages through a queue group on external NATS", func() {
			client := newExternalClient()
			ctx := testTenantCtx("acme-corp")

			subject, err := natsbus.WorkSubject("acme-corp", natsbus.TaskRelate, "ext-queue-001")
			Expect(err).NotTo(HaveOccurred())

			const numMessages = 10
			var count atomic.Int64

			sub, err := client.QueueSubscribe(ctx, subject, "ext-workers", func(_ context.Context, _ *natsbus.Message) error {
				count.Add(1)
				return nil
			})
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(sub.Unsubscribe)

			// No propagation delay: the queue subscription and the publishes share
			// one connection, and NATS processes a connection's operations in order.

			for range numMessages {
				Expect(client.Publish(ctx, subject, []byte("work"))).To(Succeed())
			}

			Eventually(count.Load, 10*time.Second, 50*time.Millisecond).Should(BeNumerically(">=", int64(numMessages)))
		})
	})

	Describe("Audit Subject Delivery", func() {
		It("delivers audit-subject messages published after subscribing on external NATS", func() {
			client := newExternalClient()
			ctx := testTenantCtx("acme-corp")

			subject, err := natsbus.AuditSubject("acme-corp", natsbus.AuditDecisions, "ext-audit-001")
			Expect(err).NotTo(HaveOccurred())

			payload := []byte(`{"decision":"approved","control":"AC-1"}`)
			// Only asserts that publishing to an audit subject succeeds; the
			// message predates the subscription and its delivery is not checked.
			publishOrFail(client, ctx, subject, payload)

			// This spec does not cover JetStream persistence (natsbus.Client has no
			// stream-read API); it relies on same-connection ordering.
			received := subscribeOne(client, ctx, subject)

			payload2 := []byte(`{"decision":"denied","control":"AC-2"}`)
			publishOrFail(client, ctx, subject, payload2)

			msg := receiveOne(received, 5*time.Second)
			Expect(string(msg.Data)).To(Equal(string(payload2)))
		})
	})
})

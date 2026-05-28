//go:build integration_nats

package natsbus_test

import (
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/tlsconfig"
)

func TestMain(m *testing.M) {
	// Require NATS URL to be set for external tests
	if os.Getenv("TEST_NATS_URL") == "" {
		// Skip: no external NATS available
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func loadTLSConfig(t *testing.T) *config.TLSConfig {
	t.Helper()

	caFile := os.Getenv("TEST_NATS_CA")
	certFile := os.Getenv("TEST_NATS_CERT")
	keyFile := os.Getenv("TEST_NATS_KEY")

	if caFile == "" || certFile == "" || keyFile == "" {
		t.Skip("TEST_NATS_CA, TEST_NATS_CERT, TEST_NATS_KEY must be set for TLS tests")
	}

	return &config.TLSConfig{
		Mode: "mutual",
		CA:   caFile,
		Cert: certFile,
		Key:  keyFile,
	}
}

func newExternalClient(t *testing.T) natsbus.Client {
	t.Helper()

	natsURL := os.Getenv("TEST_NATS_URL")
	tlsCfgInput := loadTLSConfig(t)

	tlsCfg, err := tlsconfig.BuildTLSConfig(*tlsCfgInput, "nats")
	if err != nil {
		t.Fatalf("build TLS config: %v", err)
	}

	cfg := config.NATSConfig{
		URL:     natsURL,
		TLS:     true,
		Streams: defaultTestStreamsConfig(),
	}

	client, err := natsbus.New(cfg, natsbus.WithTLSConfig(tlsCfg))
	if err != nil {
		t.Fatalf("failed to create external client: %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("close error: %v", err)
		}
	})
	return client
}

func TestExternalTLSConnection(t *testing.T) {
	client := newExternalClient(t)
	ctx := testTenantCtx(t, "acme-corp")

	subject, err := natsbus.WorkSubject("acme-corp", natsbus.TaskClassify, "ext-tls-001")
	if err != nil {
		t.Fatalf("subject: %v", err)
	}

	received := subscribeOne(t, client, ctx, subject)

	payload := []byte(`{"test":"tls"}`)
	publishOrFail(t, client, ctx, subject, payload)

	msg := receiveOne(t, received, 5*time.Second)
	if string(msg.Data) != string(payload) {
		t.Errorf("data = %q, want %q", msg.Data, payload)
	}
}

func TestExternalProvenanceHeaders(t *testing.T) {
	client := newExternalClient(t)
	ctx := testTenantCtx(t, "test-tenant")

	subject, err := natsbus.WorkSubject("test-tenant", natsbus.TaskEmbed, "ext-prov-001")
	if err != nil {
		t.Fatalf("subject: %v", err)
	}

	received := subscribeOne(t, client, ctx, subject)

	payload := []byte("external provenance test")
	publishOrFail(t, client, ctx, subject, payload)

	msg := receiveOne(t, received, 5*time.Second)
	assertProvenanceHeaders(t, msg, payload, "test-tenant")
}

func TestExternalQueueGroup(t *testing.T) {
	client := newExternalClient(t)
	ctx := testTenantCtx(t, "acme-corp")

	subject, err := natsbus.WorkSubject("acme-corp", natsbus.TaskRelate, "ext-queue-001")
	if err != nil {
		t.Fatalf("subject: %v", err)
	}

	const numMessages = 10
	var count atomic.Int64

	sub, err := client.QueueSubscribe(ctx, subject, "ext-workers", func(msg *natsbus.Message) error {
		count.Add(1)
		return nil
	})
	if err != nil {
		t.Fatalf("queue subscribe: %v", err)
	}
	defer sub.Unsubscribe()

	time.Sleep(100 * time.Millisecond)

	for range numMessages {
		if err := client.Publish(ctx, subject, []byte("work")); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	deadline := time.After(10 * time.Second)
	for count.Load() < numMessages {
		select {
		case <-deadline:
			t.Fatalf("timeout: received %d of %d messages", count.Load(), numMessages)
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}
}

func TestExternalJetStreamAudit(t *testing.T) {
	client := newExternalClient(t)
	ctx := testTenantCtx(t, "acme-corp")

	subject, err := natsbus.AuditSubject("acme-corp", natsbus.AuditDecisions, "ext-audit-001")
	if err != nil {
		t.Fatalf("subject: %v", err)
	}

	payload := []byte(`{"decision":"approved","control":"AC-1"}`)
	publishOrFail(t, client, ctx, subject, payload)

	// Verify stream exists (audit stream should have been created on startup)
	received := subscribeOne(t, client, ctx, subject)

	// Publish another to verify the stream is working
	payload2 := []byte(`{"decision":"denied","control":"AC-2"}`)
	publishOrFail(t, client, ctx, subject, payload2)

	msg := receiveOne(t, received, 5*time.Second)
	if string(msg.Data) != string(payload2) {
		t.Errorf("data = %q, want %q", msg.Data, payload2)
	}
}

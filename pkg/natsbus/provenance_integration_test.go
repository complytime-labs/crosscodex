//go:build integration

package natsbus

import (
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/nats-io/nats.go"
)

// startRawNATSConn starts an embedded NATS server and returns a raw
// nats.Conn that bypasses natsbus entirely, plus a cleanup function.
func startRawNATSConn(t *testing.T, storeSuffix string) *nats.Conn {
	t.Helper()

	storeDir := filepath.Join(t.TempDir(), storeSuffix)
	cfg := config.NATSEmbeddedConfig{StoreDir: storeDir}
	logger := slog.Default()

	es, err := startEmbedded(cfg, logger)
	if err != nil {
		t.Fatalf("start embedded: %v", err)
	}
	t.Cleanup(es.shutdown)

	rawConn, err := nats.Connect(es.clientURL())
	if err != nil {
		t.Fatalf("raw connect: %v", err)
	}
	t.Cleanup(rawConn.Close)

	return rawConn
}

// subscribeRejectingProvenance sets up a natsbus wrapHandler subscriber
// on a raw NATS connection. Returns a flag indicating whether the handler
// was called (should remain false for messages lacking provenance).
func subscribeRejectingProvenance(t *testing.T, rawConn *nats.Conn, subject string) *atomic.Bool {
	t.Helper()

	var handlerCalled atomic.Bool
	c := &client{
		conn: rawConn,
		opts: defaultClientOptions(),
	}
	wrappedHandler := c.wrapHandler(func(msg *Message) error {
		handlerCalled.Store(true)
		return nil
	})

	sub, err := rawConn.Subscribe(subject, wrappedHandler)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { sub.Unsubscribe() })

	return &handlerCalled
}

// TestRawPublishRejectedWithoutProvenance proves that a message published
// directly via a raw NATS connection (bypassing natsbus.Publish) is
// rejected by a natsbus subscriber because it lacks provenance headers.
// This enforces "fail closed" for provenance: all messages flowing through
// the system must originate from natsbus.
func TestRawPublishRejectedWithoutProvenance(t *testing.T) {
	rawConn := startRawNATSConn(t, "nats-provenance-test")
	subject := "crosscodex.work.test-tenant.classify.job-raw"
	handlerCalled := subscribeRejectingProvenance(t, rawConn, subject)

	// Publish a raw message with NO headers (no provenance).
	if err := rawConn.Publish(subject, []byte("raw payload without provenance")); err != nil {
		t.Fatalf("raw publish: %v", err)
	}
	rawConn.Flush()

	// Wait long enough for delivery.
	time.Sleep(500 * time.Millisecond)

	if handlerCalled.Load() {
		t.Fatal("handler was called for a message without provenance headers; expected rejection")
	}
}

// TestRawPublishWithPartialProvenanceRejected proves that a message with
// only some provenance headers is also rejected.
func TestRawPublishWithPartialProvenanceRejected(t *testing.T) {
	rawConn := startRawNATSConn(t, "nats-partial-provenance-test")
	subject := "crosscodex.work.test-tenant.classify.job-partial"
	handlerCalled := subscribeRejectingProvenance(t, rawConn, subject)

	// Publish with only tenant header — missing the other 4 required headers.
	msg := &nats.Msg{
		Subject: subject,
		Data:    []byte("partial provenance"),
		Header:  nats.Header{"X-Tenant-Id": []string{"test-tenant"}},
	}
	if err := rawConn.PublishMsg(msg); err != nil {
		t.Fatalf("raw publish: %v", err)
	}
	rawConn.Flush()

	time.Sleep(500 * time.Millisecond)

	if handlerCalled.Load() {
		t.Fatal("handler was called for a message with partial provenance headers; expected rejection")
	}
}

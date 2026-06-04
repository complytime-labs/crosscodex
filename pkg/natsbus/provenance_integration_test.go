//go:build integration

package natsbus

import (
	"context"
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
	wrappedHandler := c.wrapHandler(func(_ context.Context, msg *Message) error {
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

// TestContentHashMismatchRejected proves that a message with valid
// provenance headers but a tampered content hash is rejected by
// wrapHandler. The handler is never called.
func TestContentHashMismatchRejected(t *testing.T) {
	rawConn := startRawNATSConn(t, "nats-hash-mismatch-test")
	subject := "crosscodex.work.test-tenant.classify.job-hash"

	var handlerCalled atomic.Bool
	c := &client{
		conn: rawConn,
		opts: defaultClientOptions(),
	}
	wrappedHandler := c.wrapHandler(func(_ context.Context, _ *Message) error {
		handlerCalled.Store(true)
		return nil
	})

	sub, err := rawConn.Subscribe(subject, wrappedHandler)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { sub.Unsubscribe() })

	// Build a message with all 5 provenance headers but a wrong content hash.
	payload := []byte("legitimate payload")
	wrongHash := contentHash([]byte("different payload"))
	msg := &nats.Msg{
		Subject: subject,
		Data:    payload,
		Header: nats.Header{
			HeaderTraceID:       []string{"0af7651916cd43dd8448eb211c80319c"}, // DevSkim: ignore DS173237 - OTel trace ID test fixture, not a credential
			HeaderSpanID:        []string{"b7ad6b7169203331"},
			HeaderTenantID:      []string{"test-tenant"},
			HeaderTimestamp:     []string{time.Now().UTC().Format(time.RFC3339Nano)},
			HeaderContentSHA256: []string{wrongHash},
		},
	}
	if err := rawConn.PublishMsg(msg); err != nil {
		t.Fatalf("raw publish: %v", err)
	}
	rawConn.Flush()

	time.Sleep(500 * time.Millisecond)

	if handlerCalled.Load() {
		t.Fatal("handler was called for a message with mismatched content hash; expected rejection")
	}
}

// TestContentHashMatchAccepted proves that a message with valid
// provenance headers and a correct content hash is accepted.
func TestContentHashMatchAccepted(t *testing.T) {
	rawConn := startRawNATSConn(t, "nats-hash-match-test")
	subject := "crosscodex.work.test-tenant.classify.job-hash-ok"

	var handlerCalled atomic.Bool
	c := &client{
		conn: rawConn,
		opts: defaultClientOptions(),
	}
	wrappedHandler := c.wrapHandler(func(_ context.Context, _ *Message) error {
		handlerCalled.Store(true)
		return nil
	})

	sub, err := rawConn.Subscribe(subject, wrappedHandler)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { sub.Unsubscribe() })

	payload := []byte("legitimate payload")
	correctHash := contentHash(payload)
	msg := &nats.Msg{
		Subject: subject,
		Data:    payload,
		Header: nats.Header{
			HeaderTraceID:       []string{"0af7651916cd43dd8448eb211c80319c"}, // DevSkim: ignore DS173237 - OTel trace ID test fixture, not a credential
			HeaderSpanID:        []string{"b7ad6b7169203331"},
			HeaderTenantID:      []string{"test-tenant"},
			HeaderTimestamp:     []string{time.Now().UTC().Format(time.RFC3339Nano)},
			HeaderContentSHA256: []string{correctHash},
		},
	}
	if err := rawConn.PublishMsg(msg); err != nil {
		t.Fatalf("raw publish: %v", err)
	}
	rawConn.Flush()

	time.Sleep(500 * time.Millisecond)

	if !handlerCalled.Load() {
		t.Fatal("handler was NOT called for a message with correct content hash; expected acceptance")
	}
}

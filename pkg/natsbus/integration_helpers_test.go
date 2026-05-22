//go:build integration || integration_nats

package natsbus_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// defaultTestStreamsConfig returns the streams configuration shared by all
// integration tests that create a NATSConfig inline.
func defaultTestStreamsConfig() config.NATSStreamsConfig {
	return config.NATSStreamsConfig{
		AuditLLMRetention:    24 * time.Hour,
		AuditEventsRetention: 24 * time.Hour,
	}
}

// testTenantCtx creates a context with the given tenant ID for testing.
func testTenantCtx(t *testing.T, tenantID string) context.Context {
	t.Helper()
	return tenant.WithTenant(context.Background(), tenantID)
}

// subscribeOne sets up a buffered-channel subscriber on the given subject
// and returns the channel. The subscription is cleaned up via t.Cleanup.
func subscribeOne(t *testing.T, client natsbus.Client, ctx context.Context, subject string) <-chan *natsbus.Message {
	t.Helper()
	received := make(chan *natsbus.Message, 1)
	sub, err := client.Subscribe(ctx, subject, func(msg *natsbus.Message) error {
		received <- msg
		return nil
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { sub.Unsubscribe() })
	return received
}

// receiveOne waits up to timeout for a single message on ch or fails the test.
func receiveOne(t *testing.T, ch <-chan *natsbus.Message, timeout time.Duration) *natsbus.Message {
	t.Helper()
	select {
	case msg := <-ch:
		return msg
	case <-time.After(timeout):
		t.Fatal("timeout waiting for message")
		return nil // unreachable
	}
}

// publishOrFail publishes a payload to the given subject or fails the test.
func publishOrFail(t *testing.T, client natsbus.Client, ctx context.Context, subject string, payload []byte) {
	t.Helper()
	if err := client.Publish(ctx, subject, payload); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

// assertProvenanceHeaders checks that all five required provenance headers
// are present and that the content hash matches the payload.
func assertProvenanceHeaders(t *testing.T, msg *natsbus.Message, payload []byte, wantTenant string) {
	t.Helper()

	if msg.Metadata.TenantID != wantTenant {
		t.Errorf("TenantID = %q, want %q", msg.Metadata.TenantID, wantTenant)
	}

	h := sha256.Sum256(payload)
	wantHash := hex.EncodeToString(h[:])
	if msg.Metadata.ContentHash != wantHash {
		t.Errorf("ContentHash = %q, want %q", msg.Metadata.ContentHash, wantHash)
	}

	requiredHeaders := []string{"X-Trace-Id", "X-Span-Id", "X-Tenant-Id", "X-Timestamp", "X-Content-SHA256"}
	for _, hdr := range requiredHeaders {
		if _, ok := msg.Headers[hdr]; !ok {
			t.Errorf("missing required header %s", hdr)
		}
	}

	if ts, ok := msg.Headers["X-Timestamp"]; ok && len(ts) > 0 {
		if _, err := time.Parse(time.RFC3339Nano, ts[0]); err != nil {
			t.Errorf("X-Timestamp %q not valid RFC3339Nano: %v", ts[0], err)
		}
	}
}

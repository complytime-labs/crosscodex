//go:build integration || integration_nats

package natsbus_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// Helpers shared by the embedded (integration) and external (integration_nats)
// natsbus suites.

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
	DeferCleanup(sub.Unsubscribe)
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

// publishAndReceive publishes payload on a work subject for (tenantID, task,
// jobID) using client and returns the subject and the delivered message.
// The subscription is created before the publish on the same connection, so
// the subscription is registered before the message is published.
func publishAndReceive(client natsbus.Client, tenantID string, task natsbus.TaskType, jobID string, payload []byte) (string, *natsbus.Message) {
	ctx := testTenantCtx(tenantID)
	subject, err := natsbus.WorkSubject(tenantID, task, jobID)
	Expect(err).NotTo(HaveOccurred())

	received := subscribeOne(client, ctx, subject)
	publishOrFail(client, ctx, subject, payload)

	return subject, receiveOne(received, 5*time.Second)
}

// expectProvenanceRoundTrip publishes payload through client and asserts the
// delivered message carries valid provenance headers for tenantID.
func expectProvenanceRoundTrip(client natsbus.Client, tenantID string, task natsbus.TaskType, jobID string, payload []byte) {
	_, msg := publishAndReceive(client, tenantID, task, jobID, payload)
	assertProvenanceHeaders(msg, payload, tenantID)
}

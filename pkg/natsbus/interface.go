package natsbus

import (
	"context"
	"time"
)

// Client wraps NATS messaging operations with tenant-scoped subjects,
// provenance headers, and JetStream stream management.
//
// A single Client instance serves all tenants. Tenant ID is extracted
// from context.Context via pkg/tenant.FromContext() on every operation.
// Provenance headers (trace ID, span ID, tenant ID, timestamp, content
// hash) are injected automatically on publish, and subscribers drop any
// message without them.
//
// The only exception is the worker progress signal on WorkerProgressSubject
// (PublishWorkerProgress, SubscribeWorkerProgress): a tenant-free liveness
// tick with no payload and no headers. Only those two methods skip the tenant
// and provenance rules, and only for that subject; Publish, Subscribe and
// QueueSubscribe apply them to it like to any other subject.
type Client interface {
	// Publish sends a message to the specified subject.
	// Provenance headers are injected automatically.
	Publish(ctx context.Context, subject string, data []byte) error

	// PublishDurable publishes through JetStream and waits for the stream's
	// acknowledgement, so a nil error means the message is stored. Headers and
	// provenance are as for PublishWithHeaders. The wait is bounded by
	// nats.publish_ack_timeout. Errors wrap ErrPublishFailed and ErrNotStored,
	// including when no stream captures subject. Use it for messages that must
	// not be lost (audit, materialization events); Publish is fire-and-forget.
	PublishDurable(ctx context.Context, subject string, data []byte, headers map[string][]string) error

	// PublishWithHeaders sends a message with additional custom headers.
	// Provenance headers are injected automatically and merged with
	// the provided headers. Provenance headers take precedence on conflict.
	PublishWithHeaders(ctx context.Context, subject string, data []byte, headers map[string][]string) error

	// Subscribe creates a subscription to the specified subject.
	// The handler is called for each received message.
	Subscribe(ctx context.Context, subject string, handler MessageHandler) (Subscription, error)

	// QueueSubscribe creates a queue group subscription. Messages matching
	// the subject are distributed round-robin across subscribers in the
	// same queue group.
	QueueSubscribe(ctx context.Context, subject string, queue string, handler MessageHandler) (Subscription, error)

	// ConsumeDurable creates or updates the durable pull consumer cfg describes
	// (explicit acks) and calls handler for each message until the returned
	// Subscription stops. The handler's context carries the message's tenant
	// (validated) and the publisher's trace context. Outcome: nil acks; an
	// error wrapping ErrPermanent terminates the message (removed from a
	// work-queue stream); any other error redelivers it after
	// cfg.RedeliverDelay, up to cfg.MaxDeliver. Message.Metadata carries
	// Stream, Sequence and NumDelivered. A message that fails the provenance or
	// content-hash check is terminated and counted, never delivered to handler.
	// Unsubscribe stops at once; Drain stops fetching and lets in-flight messages
	// finish but does not wait for them.
	ConsumeDurable(ctx context.Context, cfg ConsumerConfig, handler MessageHandler) (Subscription, error)

	// SubjectMessageCount returns how many messages stream holds on subject,
	// which may be a wildcard pattern. A work-queue stream holds only messages
	// not yet acknowledged, so a nonzero count means work is still queued.
	// Errors wrap ErrStreamNotFound when stream does not exist.
	SubjectMessageCount(ctx context.Context, stream, subject string) (uint64, error)

	// PublishWorkerProgress publishes one tick on WorkerProgressSubject with
	// an empty payload and no headers. It needs no tenant in ctx, which is
	// used only for the local span; nothing from ctx goes on the wire.
	PublishWorkerProgress(ctx context.Context) error

	// SubscribeWorkerProgress calls handler once for every message on
	// WorkerProgressSubject, from every publisher. The handler receives
	// nothing from the message, so no wire data reaches it; a call means only
	// "a worker finished a task somewhere". Unlike Subscribe it skips the
	// provenance check, which a tick cannot pass. Every subscriber gets every
	// tick (no queue group).
	SubscribeWorkerProgress(ctx context.Context, handler func()) (Subscription, error)

	// CreateStream creates or updates a JetStream stream.
	// Returns nil if the stream already exists with matching configuration.
	CreateStream(ctx context.Context, config StreamConfig) error

	// DeleteStream removes a JetStream stream and all its messages.
	DeleteStream(ctx context.Context, name string) error

	// AuditStreamRetention reads the live MaxAge of each audit stream from the
	// server, keyed by stream name ("AUDIT_LLM", "AUDIT_DECISIONS",
	// "AUDIT_EVENTS"). It is used to detect drift from the configured retention.
	AuditStreamRetention(ctx context.Context) (map[string]time.Duration, error)

	// Close drains the connection, stops the embedded server if applicable,
	// and releases all resources. Safe to call multiple times.
	Close() error
}

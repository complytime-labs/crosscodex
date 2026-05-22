package natsbus

import "context"

// Client wraps NATS messaging operations with tenant-scoped subjects,
// provenance headers, and JetStream stream management.
//
// A single Client instance serves all tenants. Tenant ID is extracted
// from context.Context via pkg/tenant.FromContext() on every operation.
// Provenance headers (trace ID, span ID, tenant ID, timestamp, content
// hash) are injected automatically on publish.
type Client interface {
	// Publish sends a message to the specified subject.
	// Provenance headers are injected automatically.
	Publish(ctx context.Context, subject string, data []byte) error

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

	// CreateStream creates or updates a JetStream stream.
	// Returns nil if the stream already exists with matching configuration.
	CreateStream(ctx context.Context, config StreamConfig) error

	// DeleteStream removes a JetStream stream and all its messages.
	DeleteStream(ctx context.Context, name string) error

	// Close drains the connection, stops the embedded server if applicable,
	// and releases all resources. Safe to call multiple times.
	Close() error
}

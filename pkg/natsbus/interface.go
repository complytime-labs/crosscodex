package natsbus

import "context"

// Client wraps NATS JetStream operations.
//
// Implementations must handle tenant isolation by prefixing subjects
// with tenant IDs and using separate streams per tenant.
type Client interface {
	// Publish sends a message to the specified subject.
	Publish(ctx context.Context, subject string, data []byte) error

	// PublishWithHeaders sends a message with custom headers.
	PublishWithHeaders(ctx context.Context, subject string, data []byte, headers map[string]string) error

	// Subscribe creates a subscription to the specified subject.
	// The handler is called for each received message.
	Subscribe(ctx context.Context, subject string, handler MessageHandler) (Subscription, error)

	// CreateStream creates a new JetStream stream.
	// Returns nil if the stream already exists with matching configuration.
	CreateStream(ctx context.Context, config StreamConfig) error

	// DeleteStream removes a JetStream stream and all its messages.
	DeleteStream(ctx context.Context, name string) error

	// Close closes the connection and releases resources.
	Close() error
}

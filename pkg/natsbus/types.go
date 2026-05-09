package natsbus

// Message represents a received NATS message.
type Message struct {
	Subject  string            // Message subject
	Data     []byte            // Message payload
	Headers  map[string]string // Message headers
	Metadata MessageMetadata   // JetStream metadata
}

// MessageMetadata holds JetStream-specific metadata.
type MessageMetadata struct {
	Stream       string // Stream name
	Sequence     uint64 // Sequence number
	Timestamp    int64  // Unix timestamp
	NumDelivered uint64 // Delivery attempt count
}

// MessageHandler processes received messages.
type MessageHandler func(msg *Message) error

// StreamConfig defines a JetStream stream.
type StreamConfig struct {
	Name        string   // Stream name
	Subjects    []string // Subject patterns
	MaxMessages int64    // Maximum message count
	MaxBytes    int64    // Maximum total size
	MaxAge      int64    // Maximum message age in seconds
	Replicas    int      // Replication factor
}

// Subscription represents an active message subscription.
type Subscription interface {
	// Unsubscribe stops receiving messages and releases resources.
	Unsubscribe() error

	// Drain stops receiving new messages but processes outstanding ones.
	Drain() error
}

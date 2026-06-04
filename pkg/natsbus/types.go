package natsbus

import (
	"context"
	"time"
)

// Stage represents a pipeline stage event.
type Stage string

const (
	StageStarted   Stage = "started"
	StageCompleted Stage = "completed"
	StageFailed    Stage = "failed"
)

// TaskType represents the kind of LLM work task.
type TaskType string

const (
	TaskClassify  TaskType = "classify"
	TaskRelate    TaskType = "relate"
	TaskRequires  TaskType = "requires"
	TaskArtifacts TaskType = "artifacts"
	TaskEmbed     TaskType = "embed"
)

// AuditType represents the kind of audit record.
type AuditType string

const (
	AuditLLM       AuditType = "llm"
	AuditDecisions AuditType = "decisions"
	AuditEvents    AuditType = "events"
)

// Message represents a received NATS message.
type Message struct {
	Subject  string              // Message subject
	Data     []byte              // Message payload
	Headers  map[string][]string // Message headers (multi-value)
	Metadata MessageMetadata     // JetStream and provenance metadata
}

// MessageMetadata holds JetStream-specific and provenance metadata.
type MessageMetadata struct {
	Stream       string    // Stream name (empty for core NATS)
	Sequence     uint64    // JetStream sequence number
	Timestamp    time.Time // Message timestamp
	NumDelivered uint64    // Delivery attempt count
	// Provenance fields extracted from headers.
	TraceID     string // OpenTelemetry trace ID
	SpanID      string // OpenTelemetry span ID
	TenantID    string // Tenant ID from X-Tenant-Id header
	ContentHash string // SHA-256 of Data from X-Content-SHA256 header
}

// MessageHandler processes received messages.
// The context carries the reconstructed OTel trace context from the
// publisher's provenance headers, enabling trace continuity across NATS.
type MessageHandler func(ctx context.Context, msg *Message) error

// StreamConfig defines a JetStream stream.
type StreamConfig struct {
	Name     string        // Stream name
	Subjects []string      // Subject patterns
	MaxAge   time.Duration // Maximum message age; 0 = unlimited
	Replicas int           // Replication factor
}

// Subscription represents an active message subscription.
type Subscription interface {
	// Unsubscribe stops receiving messages and releases resources.
	Unsubscribe() error

	// Drain stops receiving new messages but processes outstanding ones.
	Drain() error
}

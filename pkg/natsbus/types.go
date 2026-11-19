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
	TaskCritique  TaskType = "critique"
)

// X-Error values a worker publishes on a failed task result. They are the
// worker/collector contract: the collector matches a code exactly (never
// the text of an error), and any other value is an unclassified failure.
// The collector's retry rule for each code is in internal/analysis.
const (
	// ResultErrorOutputTruncated: the model gave no answer within
	// max_tokens (llmclient.ErrOutputTruncated). Never retried: the same
	// request at the same budget gets the same result.
	ResultErrorOutputTruncated = "output_truncated"
	// ResultErrorTimeout: the LLM call timed out (llmclient.ErrTimeout).
	// Retried only when analysis.engine.retry_on_timeout is set, because
	// the provider may already have billed the call.
	ResultErrorTimeout = "timeout"
	// ResultErrorRateLimited: the provider rate-limited the call past
	// every gateway attempt (llmclient.ErrRateLimitExceeded). Retried
	// with backoff, up to analysis.engine.max_retries.
	ResultErrorRateLimited = "rate_limited"
	// ResultErrorInvalidPayload: the task payload was malformed. Never
	// retried: the same payload fails the same way.
	ResultErrorInvalidPayload = "invalid_payload"
)

// taskTypes lists every TaskType constant, in declaration order.
var taskTypes = []TaskType{TaskClassify, TaskRelate, TaskRequires, TaskArtifacts, TaskEmbed, TaskCritique}

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
	Name      string        // Stream name
	Subjects  []string      // Subject patterns
	MaxAge    time.Duration // Maximum message age; 0 = unlimited
	Replicas  int           // Replication factor
	WorkQueue bool          // true: work-queue retention (a message is removed once acknowledged); false: limits retention
}

// Subscription represents an active message subscription.
type Subscription interface {
	// Unsubscribe stops receiving messages and releases resources.
	Unsubscribe() error

	// Drain stops receiving new messages but processes outstanding ones.
	Drain() error
}

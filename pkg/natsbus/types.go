package natsbus

import (
	"context"
	"fmt"
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
	// ResultErrorAuditFailed: the LLM call completed but its audit record
	// was not stored (llmclient.ErrAuditFailed). Never retried: the call was
	// made and probably billed.
	ResultErrorAuditFailed = "audit_failed"
	// ResultErrorTenantMismatch: the task's provenance tenant header
	// differed from the tenant in its work subject, so the worker refused it
	// before any LLM call. Never retried: the same message fails the same
	// way, and it signals a misrouted or forged message, not a transient fault.
	ResultErrorTenantMismatch = "tenant_mismatch"
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

// GraphOutcome is the JSON payload of a GraphOutcomeSubject event: the graph
// role's answer to a job's materialization barrier, or the failure of one of
// the job's materialization events on its last delivery.
type GraphOutcome struct {
	JobID    string `json:"job_id"`
	Outcome  string `json:"outcome"`  // GraphOutcomeMaterialized or GraphOutcomeFailed
	Analyzer string `json:"analyzer"` // the failed event's analyzer; empty on a barrier's answer
	Error    string `json:"error"`    // what failed and where to look; never driver detail, empty when materialized
}

// MaterializeBarrier is the JSON payload of a MaterializeBarrierSubject
// event. The graph role answers it with a GraphOutcome once no
// materialization event of the job is left in MaterializeStream, and drops it
// unanswered after Deadline. Both times are required.
//
// NotBefore is when the run the barrier belongs to started or resumed. A
// retried or resumed job keeps its job ID, and AUDIT_EVENTS keeps an earlier
// run's failed outcomes, so the graph role counts a failed outcome only when
// the stream stored it at or after NotBefore. The comparison is against the
// NATS server's clock, so a publisher clock running ahead of the server's
// could hide a failure stored in this run's first moments.
type MaterializeBarrier struct {
	JobID     string    `json:"job_id"`
	Deadline  time.Time `json:"deadline"`
	NotBefore time.Time `json:"not_before"`
}

// ConsumerConfig defines a durable JetStream pull consumer. The caller owns
// the consumer: ConsumeDurable creates or updates it to match this config.
type ConsumerConfig struct {
	Stream         string        // Stream to consume; must exist
	Durable        string        // Durable consumer name; state survives restarts
	FilterSubjects []string      // Subjects to receive; empty = every subject in the stream
	AckWait        time.Duration // Time to handle a message before redelivery; 0 = server default
	MaxDeliver     int           // Maximum delivery attempts per message; -1 = unlimited
	RedeliverDelay time.Duration // Delay before redelivery after a handler error (Nak)
}

// Validate reports why cfg cannot create a consumer. Errors wrap
// ErrSubscribeFailed.
func (cfg ConsumerConfig) Validate() error {
	if cfg.Stream == "" {
		return fmt.Errorf("%w: ConsumerConfig.Stream is empty; name the stream to consume", ErrSubscribeFailed)
	}
	if cfg.Durable == "" {
		return fmt.Errorf("%w: ConsumerConfig.Durable is empty; name the durable consumer", ErrSubscribeFailed)
	}
	if cfg.MaxDeliver == 0 || cfg.MaxDeliver < -1 {
		// The server treats 0 as unlimited, so a config that merely forgot the
		// field would retry a poison message forever; make that explicit.
		return fmt.Errorf("%w: ConsumerConfig.MaxDeliver is %d for consumer %s; set a positive attempt count, or -1 for unlimited",
			ErrSubscribeFailed, cfg.MaxDeliver, cfg.Durable)
	}
	return nil
}

// Subscription represents an active message subscription.
type Subscription interface {
	// Unsubscribe stops receiving messages and releases resources.
	Unsubscribe() error

	// Drain stops receiving new messages but processes outstanding ones.
	Drain() error
}

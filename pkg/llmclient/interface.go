package llmclient

import "context"

// Client calls the LLM gateway for completions and embeddings.
//
// Implementations must be safe for concurrent use by multiple goroutines.
//
// Implementations handle rate limiting, retries, and tenant-scoped
// requests. Tenant ID and job ID are carried in the request structs,
// not in context, so that callers explicitly provide them.
type Client interface {
	// Complete generates a chat completion.
	Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error)

	// Embed generates vector embeddings for the provided texts.
	Embed(ctx context.Context, req *EmbeddingRequest) (*EmbeddingResponse, error)

	// Health checks the LLM gateway availability.
	Health(ctx context.Context) error

	// Close releases client resources.
	Close() error
}

// AuditEmitter publishes LLM audit events. An LLM call must not go
// unaudited: EmitLLMAudit must return an error unless the event is stored
// durably. The client then withholds the result and returns ErrAuditFailed,
// so implementations must return every failure, never swallow it.
//
// The gateway layer provides a natsbus-backed implementation.
// The llmclient package never imports natsbus directly.
type AuditEmitter interface {
	EmitLLMAudit(ctx context.Context, event *AuditEvent) error
}

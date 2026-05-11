package llmclient

import "context"

// Client calls the LLM gateway for completions and embeddings.
//
// Implementations must handle rate limiting, retries, and
// tenant-scoped requests via headers or metadata.
type Client interface {
	// Complete generates text completions.
	Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error)

	// Embed generates vector embeddings for the provided texts.
	Embed(ctx context.Context, texts []string) ([][]float32, error)

	// Health checks the LLM gateway availability.
	Health(ctx context.Context) error

	// Close releases client resources.
	Close() error
}

package vectordb

import "context"

// Index manages vector embeddings for similarity search.
//
// Implementations must handle tenant isolation by partitioning
// embeddings by tenant ID.
type Index interface {
	// Insert adds or updates a vector embedding.
	Insert(ctx context.Context, id string, vector []float32, metadata map[string]any) error

	// Search finds the k-nearest neighbors to the query vector.
	// Returns matches ordered by similarity score (descending).
	Search(ctx context.Context, query []float32, limit int) ([]Match, error)

	// Delete removes a vector embedding by ID.
	Delete(ctx context.Context, id string) error

	// Get retrieves a vector embedding by ID.
	// Returns ErrNotFound if the embedding does not exist.
	Get(ctx context.Context, id string) (*Match, error)

	// Count returns the total number of embeddings in the index.
	Count(ctx context.Context) (int64, error)
}

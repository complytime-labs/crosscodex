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

// VectorDB provides domain-specific vector operations for compliance embeddings.
// Implementations must handle tenant isolation and model validation.
type VectorDB interface {
	// StoreEmbedding adds or updates a single embedding with compliance metadata
	StoreEmbedding(ctx context.Context, tenant string, embedding Embedding) error

	// StoreBatch efficiently stores multiple embeddings in a single operation
	StoreBatch(ctx context.Context, tenant string, embeddings []Embedding) error

	// FindSimilar searches for embeddings similar to the query vector.
	// Only searches embeddings from the specified model to ensure compatibility.
	// Returns results ordered by similarity score (descending).
	FindSimilar(ctx context.Context, tenant string, query FindSimilarQuery) ([]SimilarityResult, error)

	// DeleteByModel removes all embeddings for a specific catalog and model.
	// Useful for reprocessing when switching embedding models.
	DeleteByModel(ctx context.Context, tenant, catalogID, model string) error
}

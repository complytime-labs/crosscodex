package vectordb

// Match represents a similarity search result.
type Match struct {
	ID       string         // Document identifier
	Score    float32        // Similarity score (0.0 to 1.0)
	Vector   []float32      // Document embedding
	Metadata map[string]any // Associated metadata
}

// DistanceMetric defines how vector similarity is calculated.
type DistanceMetric string

const (
	// MetricCosine uses cosine similarity.
	MetricCosine DistanceMetric = "cosine"

	// MetricEuclidean uses Euclidean distance.
	MetricEuclidean DistanceMetric = "euclidean"

	// MetricDotProduct uses dot product similarity.
	MetricDotProduct DistanceMetric = "dot_product"
)

// Embedding represents a document embedding with compliance-specific metadata
type Embedding struct {
	CatalogID string         // Required - catalog identifier (e.g., "nist-800-53")
	ControlID string         // Required - control/document identifier
	Model     string         // Required - embedding model used
	Vector    []float32      // Required - the embedding vector
	Metadata  map[string]any // Optional - extensible metadata for document types
}

// FindSimilarQuery specifies parameters for similarity search
type FindSimilarQuery struct {
	CatalogID string    // Required - filter to specific catalog
	Model     string    // Required - only search embeddings from this model
	Vector    []float32 // Required - query vector for similarity comparison
	Limit     int       // Required - maximum results to return (must be > 0)
}

// SimilarityResult represents a similarity search match
type SimilarityResult struct {
	ControlID  string         // Document identifier that matched
	Similarity float32        // Cosine similarity score (0.0 to 1.0, higher = more similar)
	Metadata   map[string]any // Associated metadata for result context
}

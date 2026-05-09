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

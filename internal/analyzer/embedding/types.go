package embedding

// SimilarityMatrix holds a cosine similarity matrix with labeled rows/columns.
type SimilarityMatrix struct {
	// IDs are the row and column identifiers (control IDs), in order.
	IDs []string

	// Values is a dense row-major matrix. Values[i][j] is the cosine
	// similarity between IDs[i] and IDs[j], scaled to [0, 100].
	Values [][]float32
}

// SimilarityPair represents a pair of controls with their similarity score.
type SimilarityPair struct {
	SourceID   string
	TargetID   string
	Similarity float32 // Cosine similarity score in [0, 100]
}

package llmclient

// CompletionRequest represents a text completion request.
type CompletionRequest struct {
	Prompt      string   // Input prompt
	MaxTokens   int      // Maximum tokens to generate
	Temperature float64  // Sampling temperature (0.0 to 1.0)
	StopWords   []string // Stop sequences
}

// CompletionResponse represents the LLM response.
type CompletionResponse struct {
	Text         string // Generated text
	TokensUsed   int    // Total tokens consumed
	FinishReason string // Reason completion stopped ("stop", "length", etc.)
}

// EmbeddingRequest represents a request for text embeddings.
type EmbeddingRequest struct {
	Texts []string // Texts to embed
	Model string   // Embedding model name
}

// EmbeddingResponse represents embedding vectors.
type EmbeddingResponse struct {
	Embeddings [][]float32 // Embedding vectors (one per input text)
	Dimensions int         // Embedding dimension
}

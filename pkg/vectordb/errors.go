package vectordb

import "errors"

var (
	// ErrNotFound indicates the embedding does not exist.
	ErrNotFound = errors.New("embedding not found")

	// ErrInvalidDimension indicates the vector dimension does not match the index.
	ErrInvalidDimension = errors.New("invalid vector dimension")

	// ErrIndexNotFound indicates the specified index does not exist.
	ErrIndexNotFound = errors.New("index not found")

	// ErrIncompatibleModel indicates query model doesn't match stored embeddings
	ErrIncompatibleModel = errors.New("query model does not match stored embeddings")

	// ErrModelNotFound indicates no embeddings exist for the specified model
	ErrModelNotFound = errors.New("no embeddings found for specified model")
)

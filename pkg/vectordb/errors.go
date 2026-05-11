package vectordb

import "errors"

var (
	// ErrNotFound indicates the embedding does not exist.
	ErrNotFound = errors.New("embedding not found")

	// ErrInvalidDimension indicates the vector dimension does not match the index.
	ErrInvalidDimension = errors.New("invalid vector dimension")

	// ErrIndexNotFound indicates the specified index does not exist.
	ErrIndexNotFound = errors.New("index not found")
)

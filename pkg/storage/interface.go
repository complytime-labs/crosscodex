package storage

import (
	"context"
	"io"
)

// Provider abstracts object storage operations across different backends.
//
// Implementations must be safe for concurrent use and handle tenant isolation
// if multi-tenancy is enabled.
type Provider interface {
	// Get retrieves an object by key.
	// Returns ErrNotFound if the object does not exist.
	Get(ctx context.Context, key string) (io.ReadCloser, error)

	// Put stores an object at the specified key.
	// Overwrites existing objects.
	Put(ctx context.Context, key string, data io.Reader) error

	// Delete removes an object by key.
	// Returns nil if the object does not exist (idempotent).
	Delete(ctx context.Context, key string) error

	// List returns metadata for all objects matching the prefix.
	List(ctx context.Context, prefix string) ([]ObjectMetadata, error)

	// Exists reports whether an object with the given key exists.
	Exists(ctx context.Context, key string) (bool, error)

	// Stat retrieves metadata for a single object.
	// Returns ErrNotFound if the object does not exist.
	Stat(ctx context.Context, key string) (*ObjectMetadata, error)

	// Close releases resources held by the provider.
	Close() error
}

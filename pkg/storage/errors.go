package storage

import "errors"

var (
	// ErrNotFound indicates the requested object does not exist.
	ErrNotFound = errors.New("object not found")

	// ErrInvalidKey indicates the object key is invalid.
	ErrInvalidKey = errors.New("invalid object key")

	// ErrProviderClosed indicates the provider has been closed.
	ErrProviderClosed = errors.New("storage provider closed")

	// ErrBucketRequired indicates the bucket name was empty.
	ErrBucketRequired = errors.New("bucket name is required")
)

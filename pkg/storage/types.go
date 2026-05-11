package storage

// ProviderType identifies the storage backend type.
type ProviderType string

const (
	// ProviderTypeLocal indicates local filesystem storage.
	ProviderTypeLocal ProviderType = "local"

	// ProviderTypeS3 indicates S3-compatible object storage.
	ProviderTypeS3 ProviderType = "s3"
)

// ObjectMetadata holds metadata about a stored object.
type ObjectMetadata struct {
	Key          string            // Object key
	Size         int64             // Size in bytes
	ContentType  string            // MIME content type
	LastModified int64             // Unix timestamp
	ETag         string            // Entity tag
	Metadata     map[string]string // Custom metadata
}

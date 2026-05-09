// Package storage provides an abstraction layer for object storage.
//
// Supports both local filesystem and S3-compatible object storage through
// a unified Provider interface.
//
// Example usage:
//
//	provider, err := storage.NewLocal("/var/lib/crosscodex/data")
//	if err != nil {
//	    return err
//	}
//	defer provider.Close()
//
//	err = provider.Put(ctx, "documents/doc1.json", bytes.NewReader(data))
package storage

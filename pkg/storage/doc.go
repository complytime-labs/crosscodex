// Package storage provides an abstraction layer for object storage with
// tenant-scoped isolation.
//
// Each Provider instance is bound to a single tenant at construction time.
// All operations are automatically scoped to the tenant's namespace.
//
// Local filesystem example:
//
//	provider, err := storage.NewLocal("/var/lib/crosscodex/data", "tenant-1")
//	if err != nil {
//	    return err
//	}
//	defer provider.Close()
//
//	err = provider.Put(ctx, "documents/doc1.json", bytes.NewReader(data))
//
// S3-compatible example:
//
//	provider, err := storage.NewS3("my-bucket", "tenant-1",
//	    storage.WithRegion("us-east-1"),
//	)
//	if err != nil {
//	    return err
//	}
//	defer provider.Close()
//
// Configuration-driven example:
//
//	provider, err := storage.NewFromConfig(cfg.Storage.Objects, "tenant-1")
//	if err != nil {
//	    return err
//	}
//	defer provider.Close()
//
// Check if an object exists before fetching:
//
//	exists, err := provider.Exists(ctx, "documents/doc1.json")
//
// Content-addressed storage for attestation bundles:
//
//	key := storage.ContentKey(bundleBytes)
//	err = provider.Put(ctx, key, bytes.NewReader(bundleBytes))
package storage

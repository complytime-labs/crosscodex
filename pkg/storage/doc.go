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
// A daemon serving many tenants builds one Provider that routes each call to
// the tenant's own backend, chosen from storage.objects.backend. The tenant
// comes from the context:
//
//	router := storage.NewTenantRouter(func(tenantID string) (storage.Provider, error) {
//	    return storage.NewFromConfig(cfg.Storage.Objects, tenantID,
//	        storage.WithTelemetry(tracer, meter))
//	})
//	defer router.Close()
//
//	ctx, err := tenant.WithTenant(ctx, "tenant-1")
//	err = router.Put(ctx, "documents/doc1.json", bytes.NewReader(data))
//
// The router opens each tenant's provider on first use and owns it; Close
// closes them all. A context without a tenant is rejected.
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

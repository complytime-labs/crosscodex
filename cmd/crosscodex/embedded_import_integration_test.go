//go:build integration

package main

import (
	"context"
	"os"
	"testing"

	"github.com/complytime-labs/crosscodex/internal/catalog"
	"github.com/complytime-labs/crosscodex/pkg/config"
)

// TestEmbeddedCatalogImport reproduces issue #113: before the fix,
// buildEmbeddedService did not provision the "embedded" tenant, so the first
// catalogs insert failed with catalogs_tenant_id_fkey (SQLSTATE 23503).
func TestEmbeddedCatalogImport(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("TEST_DATABASE_DSN not set — run: task test:integration")
	}

	// Keep embedded local storage out of the real user data dir.
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	ctx := context.Background()
	cfg := &config.Config{}
	cfg.Database.DSN = dsn

	_, resources, err := buildEmbeddedService(ctx, cfg, nil)
	if err != nil {
		t.Fatalf("buildEmbeddedService: %v", err)
	}
	defer resources.close()

	// The embedded tenant must now exist.
	var tenantRows int
	if err := resources.dbPool.QueryRow(ctx,
		"SELECT count(*) FROM tenants WHERE tenant_id = $1", embeddedTenantID).
		Scan(&tenantRows); err != nil {
		t.Fatalf("query embedded tenant: %v", err)
	}
	if tenantRows != 1 {
		t.Fatalf("expected embedded tenant row to exist, found %d", tenantRows)
	}

	// The exact operation that failed in #113: a catalogs insert under "embedded".
	store := catalog.NewPGStore(resources.dbPool)
	rec := catalog.CatalogRecord{
		CatalogID:  "e2e-113",
		TenantID:   embeddedTenantID,
		Name:       "E2E #113 Catalog",
		Version:    "1.0",
		SourceType: "oscal",
		ObjectPath: "test-fixture",
	}
	if err := store.UpsertCatalog(ctx, rec); err != nil {
		t.Fatalf("UpsertCatalog under embedded tenant failed (regression of #113): %v", err)
	}

	// Cleanup the row this test created.
	if err := resources.dbPool.Exec(ctx,
		"DELETE FROM catalogs WHERE catalog_id = $1", "e2e-113"); err != nil {
		t.Fatalf("cleanup catalog: %v", err)
	}
}

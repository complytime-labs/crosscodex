package graph_test

import (
	"testing"

	"github.com/complytime-labs/crosscodex/internal/graph"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// FuzzParseCatalogSubject checks that every subject the catalog handler
// accepts is one catalog ingest could have published. The parser leaves
// tenant validation to the handler, so an accepted subject either carries a
// tenant that tenant.ValidateTenantID rejects (and the handler drops it) or
// is exactly what natsbus.CatalogParsedSubject builds from its parts.
func FuzzParseCatalogSubject(f *testing.F) {
	f.Add("crosscodex.catalog.test-tenant.0123456789abcdef.parsed")
	f.Add("crosscodex.catalog.x")
	f.Add("crosscodex.catalog.t.c.parsed.x")
	f.Add("crosscodex.catalog.*.*.parsed")
	f.Add("crosscodex.catalog.test-tenant..parsed")
	f.Add("crosscodex.catalog.test-tenant.>.parsed")
	f.Add("crosscodex.catalog.../..parsed")
	f.Add("crosscodex.catalog.bad tenant!.cat.parsed")

	f.Fuzz(func(t *testing.T, subject string) {
		tenantID, catalogID, err := graph.ExportParseCatalogSubject(subject)
		if err != nil {
			return
		}
		if tenant.ValidateTenantID(tenantID) != nil {
			return
		}
		built, err := natsbus.CatalogParsedSubject(tenantID, catalogID)
		if err != nil {
			t.Fatalf("accepted %q (tenant %q, catalog %q) but CatalogParsedSubject rejects it: %v", subject, tenantID, catalogID, err)
		}
		if built != subject {
			t.Fatalf("accepted %q but CatalogParsedSubject rebuilds %q", subject, built)
		}
	})
}

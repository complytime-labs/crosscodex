//go:build integration

package db_test

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
)

// The schema ties every catalog dependent to a catalog of its own tenant.
// Foreign keys ignore RLS and catalog IDs are global, so a single-column FK
// would let a row of one tenant reference another tenant's catalog.
var _ = Describe("catalog tenant foreign keys", Ordered, func() {
	var (
		ctx                context.Context
		tenantA, tenantB   string
		catalogA, catalogB string
	)

	BeforeAll(func() {
		ctx = context.Background()
		tenantA, tenantB = dbtest.TestID("catfk-a"), dbtest.TestID("catfk-b")
		catalogA, catalogB = dbtest.TestID("catfk-cat-a"), dbtest.TestID("catfk-cat-b")
		for _, t := range []struct{ tenantID, catalogID, name string }{
			{tenantA, catalogA, "Catalog FK A"},
			{tenantB, catalogB, "Catalog FK B"},
		} {
			setupTenant(t.tenantID, t.name)
			Expect(suPool.Exec(ctx,
				`INSERT INTO catalogs (catalog_id, tenant_id, name, version, source_type, object_path)
				 VALUES ($1, $2, $3, '1.0', 'oscal', 'test-fixture')`, t.catalogID, t.tenantID, t.name)).To(Succeed())
		}
	})

	AfterAll(func() {
		if suPool != nil {
			for _, id := range []string{tenantA, tenantB} {
				stmt, err := dbtest.HardDeleteTenantSQL(id)
				Expect(err).NotTo(HaveOccurred())
				Expect(suPool.Exec(ctx, stmt)).To(Succeed())
			}
		}
	})

	// insertRolledBack runs one insert as the superuser, which RLS does not
	// filter, inside a transaction that is always rolled back.
	insertRolledBack := func(stmt string, args ...any) error {
		tx, err := suPool.Begin(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer tx.Rollback() //nolint:errcheck // probe transaction, always rolled back
		return tx.Exec(ctx, stmt, args...)
	}

	inserts := map[string]string{
		"controls": `INSERT INTO controls (tenant_id, control_id, catalog_id, identifier)
			VALUES ($1, 'catfk-1', $2, 'catfk-1')`,
		"embeddings": `INSERT INTO embeddings (tenant_id, catalog_id, control_id, model, vector)
			VALUES ($1, $2, 'catfk-1', 'm', '[1,2]')`,
		"classifications": `INSERT INTO classifications (tenant_id, catalog_id, control_id, type, level)
			VALUES ($1, $2, 'catfk-1', 't', 'l')`,
	}

	DescribeTable("refuses a row that references another tenant's catalog",
		func(table string) {
			err := insertRolledBack(inserts[table], tenantA, catalogB)
			Expect(err).To(HaveOccurred())
			Expect(db.IsPgErrorCode(err, "23503")).To(BeTrue(), "want foreign_key_violation, got %v", err)
			Expect(err).To(MatchError(ContainSubstring(table + "_catalog_tenant_fkey")))

			var n int
			Expect(suPool.QueryRow(ctx,
				fmt.Sprintf("SELECT count(*) FROM %s WHERE catalog_id = ANY($1)", table),
				[]string{catalogA, catalogB}).Scan(&n)).To(Succeed())
			Expect(n).To(BeZero(), "the refused insert must leave %s unchanged", table)
		},
		Entry("controls", "controls"),
		Entry("embeddings", "embeddings"),
		Entry("classifications", "classifications"),
	)

	DescribeTable("accepts a row that references its own tenant's catalog",
		func(table string) {
			Expect(insertRolledBack(inserts[table], tenantA, catalogA)).To(Succeed())
		},
		Entry("controls", "controls"),
		Entry("embeddings", "embeddings"),
		Entry("classifications", "classifications"),
	)

	It("keys catalogs by catalog_id and also by (catalog_id, tenant_id)", func() {
		Expect(catalogConstraints(ctx, suPool, "catalogs")).To(Equal([]string{
			"catalogs_catalog_tenant_key UNIQUE (catalog_id, tenant_id)",
			"catalogs_pkey PRIMARY KEY (catalog_id)",
		}))
	})

	// pg_get_constraintdef omits the default ON DELETE NO ACTION, so an
	// exact match also proves catalog purge must still delete dependents
	// before the catalog (purge_user holds DELETE on each dependent).
	DescribeTable("references catalogs only through (catalog_id, tenant_id), without cascade",
		func(table string) {
			Expect(catalogConstraints(ctx, suPool, table)).To(Equal([]string{
				table + "_catalog_tenant_fkey FOREIGN KEY (catalog_id, tenant_id) REFERENCES catalogs(catalog_id, tenant_id)",
			}))
		},
		Entry("controls", "controls"),
		Entry("embeddings", "embeddings"),
		Entry("classifications", "classifications"),
	)
})

// catalogConstraints lists "name definition" for table's foreign keys to
// catalogs, or for catalogs itself, its primary and unique keys.
func catalogConstraints(ctx context.Context, q interface {
	Query(ctx context.Context, query string, args ...any) (db.Rows, error)
}, table string) []string {
	rows, err := q.Query(ctx, `
		SELECT conname::text, pg_get_constraintdef(oid)
		FROM pg_constraint
		WHERE conrelid = $1::regclass
		  AND CASE WHEN $1::regclass = 'public.catalogs'::regclass THEN contype IN ('p', 'u')
		           ELSE confrelid = 'public.catalogs'::regclass END
		ORDER BY conname`, "public."+table)
	Expect(err).NotTo(HaveOccurred())
	defer rows.Close() //nolint:errcheck // read-only query, errors surface through rows.Err
	var got []string
	for rows.Next() {
		var name, def string
		Expect(rows.Scan(&name, &def)).To(Succeed())
		got = append(got, name+" "+def)
	}
	Expect(rows.Err()).NotTo(HaveOccurred())
	return got
}

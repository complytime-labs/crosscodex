//go:build integration

package db_test

import (
	"context"
	"database/sql"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
)

// Migration 002 adds catalog document lineage. The specs run as the
// restricted app_user, the role the daemon uses, so RLS and GRANTs apply.
var _ = Describe("catalog documents schema (migration 002)", Ordered, func() {
	var (
		ctx                context.Context
		tenantA, tenantB   string
		catalogA, catalogB string
		legacyCatalog      string
	)

	BeforeAll(func() {
		ctx = context.Background()
		tenantA, tenantB = dbtest.TestID("catdoc-a"), dbtest.TestID("catdoc-b")
		catalogA, catalogB = dbtest.TestID("catdoc-cat-a"), dbtest.TestID("catdoc-cat-b")
		legacyCatalog = dbtest.TestID("catdoc-legacy")
		for _, t := range []struct{ tenantID, catalogID string }{
			{tenantA, catalogA},
			{tenantB, catalogB},
			{tenantA, legacyCatalog},
		} {
			setupTenant(t.tenantID, "Catalog documents "+t.tenantID)
			// A catalog written as before migration 002: no version columns.
			Expect(suPool.Exec(ctx,
				`INSERT INTO catalogs (catalog_id, tenant_id, name, version, source_type, object_path)
				 VALUES ($1, $2, 'Doc catalog', '1.0', 'oscal', 'test-fixture')`, t.catalogID, t.tenantID)).To(Succeed())
		}
		Expect(suPool.Exec(ctx,
			`INSERT INTO catalog_documents (tenant_id, document_slug, current_catalog_id) VALUES ($1, 'doc-b', $2)`,
			tenantB, catalogB)).To(Succeed())
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

	It("leaves an existing catalog readable by app_user with NULL version columns", func() {
		var slug, label, supersedes, method sql.NullString
		execAsTenant(appUserConn(), tenantA, func(tx *sql.Tx) {
			Expect(tx.QueryRowContext(ctx,
				`SELECT document_slug, version_label, supersedes_catalog_id, link_method
				 FROM catalogs WHERE catalog_id = $1`, legacyCatalog).Scan(&slug, &label, &supersedes, &method)).To(Succeed())
		})
		Expect(slug.Valid || label.Valid || supersedes.Valid || method.Valid).To(BeFalse(),
			"existing catalogs must stay valid as unversioned")
	})

	It("refuses a supersedes link to another tenant's catalog", func() {
		err := probeAsTenant(tenantA,
			`UPDATE catalogs SET supersedes_catalog_id = $1 WHERE catalog_id = $2`, catalogB, catalogA)
		Expect(err).To(HaveOccurred())
		Expect(db.IsPgErrorCode(err, "23503")).To(BeTrue(), "want foreign_key_violation, got %v", err)
		Expect(err).To(MatchError(ContainSubstring("catalogs_supersedes_tenant_fkey")))

		var supersedes sql.NullString
		Expect(suPool.QueryRow(ctx, `SELECT supersedes_catalog_id FROM catalogs WHERE catalog_id = $1`, catalogA).
			Scan(&supersedes)).To(Succeed())
		Expect(supersedes.Valid).To(BeFalse(), "the refused link must leave the catalog unchanged")
	})

	It("accepts a supersedes link within one tenant", func() {
		Expect(probeAsTenant(tenantA,
			`UPDATE catalogs SET supersedes_catalog_id = $1 WHERE catalog_id = $2`, legacyCatalog, catalogA)).To(Succeed())
	})

	DescribeTable("link_method CHECK",
		func(method string, accepted bool) {
			err := probeAsTenant(tenantA, `UPDATE catalogs SET link_method = $1 WHERE catalog_id = $2`, method, catalogA)
			if accepted {
				Expect(err).NotTo(HaveOccurred())
				return
			}
			Expect(err).To(HaveOccurred())
			Expect(db.IsPgErrorCode(err, "23514")).To(BeTrue(), "want check_violation, got %v", err)
			Expect(err).To(MatchError(ContainSubstring("catalogs_link_method_check")))
		},
		Entry("accepts declared", "declared", true),
		Entry("accepts metadata", "metadata", true),
		Entry("accepts name", "name", true),
		Entry("rejects guess", "guess", false),
		Entry("rejects the empty string", "", false),
	)

	DescribeTable("document slug CHECK on catalog_documents",
		func(slug string, accepted bool) {
			err := probeAsTenant(tenantA,
				`INSERT INTO catalog_documents (tenant_id, document_slug, current_catalog_id) VALUES ($1, $2, $3)`,
				tenantA, slug, catalogA)
			if accepted {
				Expect(err).NotTo(HaveOccurred())
				return
			}
			Expect(err).To(HaveOccurred())
			Expect(db.IsPgErrorCode(err, "23514")).To(BeTrue(), "want check_violation, got %v", err)
			Expect(err).To(MatchError(ContainSubstring("catalog_documents_slug_check")))
		},
		Entry("accepts a lowercase slug", "nist-800-53", true),
		Entry("accepts a 63-character slug", "a"+strings.Repeat("b", 62), true),
		Entry("rejects uppercase", "Doc", false),
		Entry("rejects a leading hyphen", "-doc", false),
		Entry("rejects a 64-character slug", "a"+strings.Repeat("b", 63), false),
		Entry("rejects whitespace", "my doc", false),
		Entry("rejects a trailing newline", "doc\n", false),
		Entry("rejects the empty string", "", false),
	)

	It("refuses a current_catalog_id belonging to another tenant", func() {
		err := probeAsTenant(tenantA,
			`INSERT INTO catalog_documents (tenant_id, document_slug, current_catalog_id) VALUES ($1, 'doc-x', $2)`,
			tenantA, catalogB)
		Expect(err).To(HaveOccurred())
		Expect(db.IsPgErrorCode(err, "23503")).To(BeTrue(), "want foreign_key_violation, got %v", err)
		Expect(err).To(MatchError(ContainSubstring("catalog_documents_current_tenant_fkey")))
	})

	It("hides another tenant's catalog_documents rows", func() {
		var own, other int
		execAsTenant(appUserConn(), tenantA, func(tx *sql.Tx) {
			Expect(tx.QueryRowContext(ctx, `SELECT count(*) FROM catalog_documents WHERE tenant_id = $1`, tenantB).Scan(&other)).To(Succeed())
			Expect(tx.QueryRowContext(ctx, `SELECT count(*) FROM catalog_documents`).Scan(&own)).To(Succeed())
		})
		Expect(other).To(BeZero(), "tenant A must not see tenant B's documents")
		Expect(own).To(BeZero(), "tenant A has no documents of its own, so it sees none")
	})

	It("refuses an insert that names another tenant's tenant_id", func() {
		err := probeAsTenant(tenantA,
			`INSERT INTO catalog_documents (tenant_id, document_slug, current_catalog_id) VALUES ($1, 'doc-y', $2)`,
			tenantB, catalogB)
		Expect(err).To(HaveOccurred())
		Expect(db.IsPgErrorCode(err, "42501")).To(BeTrue(), "want RLS insufficient_privilege, got %v", err)
	})

	DescribeTable("denies app_user privileges beyond SELECT, INSERT, UPDATE, DELETE",
		func(stmt string) {
			_, err := appUserConn().ExecContext(ctx, stmt)
			Expect(err).To(HaveOccurred())
			Expect(db.IsPgErrorCode(err, "42501")).To(BeTrue(), "want insufficient_privilege, got %v", err)

			var n int
			Expect(suPool.QueryRow(ctx, `SELECT count(*) FROM catalog_documents WHERE tenant_id = $1`, tenantB).Scan(&n)).To(Succeed())
			Expect(n).To(Equal(1), "the refused statement must leave catalog_documents unchanged")
		},
		Entry("TRUNCATE", "TRUNCATE TABLE catalog_documents"),
		Entry("ALTER TABLE", "ALTER TABLE catalog_documents DISABLE ROW LEVEL SECURITY"),
		Entry("CREATE TRIGGER", "CREATE TRIGGER x BEFORE DELETE ON catalog_documents FOR EACH ROW EXECUTE FUNCTION prevent_classification_mutation()"),
	)

	// purge_user deletes catalogs for retention and catalog retire. Taking a
	// version out of its chain needs it to read catalog_documents and to
	// re-point catalogs.supersedes_catalog_id; it gets exactly that and no more.
	DescribeTable("purge_user privileges on the version tables",
		func(table, privilege string, want bool) {
			var got bool
			Expect(suPool.QueryRow(ctx,
				`SELECT has_table_privilege('purge_user', $1, $2)`, "public."+table, privilege).Scan(&got)).To(Succeed())
			Expect(got).To(Equal(want))
		},
		Entry("catalog_documents SELECT is granted", "catalog_documents", "SELECT", true),
		Entry("catalog_documents INSERT is not", "catalog_documents", "INSERT", false),
		Entry("catalog_documents UPDATE is not", "catalog_documents", "UPDATE", false),
		Entry("catalog_documents DELETE is not", "catalog_documents", "DELETE", false),
		Entry("catalog_documents TRUNCATE is not", "catalog_documents", "TRUNCATE", false),
		Entry("catalog_documents TRIGGER is not", "catalog_documents", "TRIGGER", false),
		Entry("catalogs SELECT is granted", "catalogs", "SELECT", true),
		Entry("catalogs DELETE is granted", "catalogs", "DELETE", true),
		Entry("catalogs INSERT is not", "catalogs", "INSERT", false),
		Entry("catalogs table-wide UPDATE is not", "catalogs", "UPDATE", false),
		Entry("catalogs TRUNCATE is not", "catalogs", "TRUNCATE", false),
	)

	It("lets purge_user update only catalogs.supersedes_catalog_id", func() {
		var updatable []string
		rows, err := suPool.Query(ctx, `
			SELECT column_name FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = 'catalogs'
			  AND has_column_privilege('purge_user', 'public.catalogs', column_name, 'UPDATE')
			ORDER BY column_name`)
		Expect(err).NotTo(HaveOccurred())
		defer rows.Close()
		for rows.Next() {
			var col string
			Expect(rows.Scan(&col)).To(Succeed())
			updatable = append(updatable, col)
		}
		Expect(rows.Err()).NotTo(HaveOccurred())
		Expect(updatable).To(Equal([]string{"supersedes_catalog_id"}))
	})
})

// probeAsTenant runs one statement as app_user scoped to tenantID inside a
// transaction that is always rolled back, so a probe never changes state and
// a failed statement needs no cleanup. It returns the statement's error.
func probeAsTenant(tenantID, stmt string, args ...any) error {
	ctx := context.Background()
	tx, err := appUserConn().BeginTx(ctx, nil)
	Expect(err).NotTo(HaveOccurred(), "BeginTx")
	defer tx.Rollback() //nolint:errcheck // probe transaction, always rolled back
	_, err = tx.ExecContext(ctx, "SELECT set_config('app.current_tenant', $1, true)", tenantID)
	Expect(err).NotTo(HaveOccurred(), "set_config tenant")
	_, err = tx.ExecContext(ctx, stmt, args...)
	return err
}

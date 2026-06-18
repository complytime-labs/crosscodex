//go:build integration

package catalog_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/complytime-labs/crosscodex/internal/catalog"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/oscal"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// ---------------------------------------------------------------------------
// Suite bootstrap (integration tests compile separately via build tag)
// ---------------------------------------------------------------------------

func TestCatalogIntegrationBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Catalog Integration BDD Suite")
}

// ---------------------------------------------------------------------------
// Suite-level DB state
// ---------------------------------------------------------------------------

var (
	suDSN  string
	suPool db.Pool
)

// Redirect slog output to GinkgoWriter so log noise only appears on failure.
var _ = BeforeEach(func() { DeferCleanup(testspecs.RedirectLogsToGinkgo()) })

var _ = SynchronizedBeforeSuite(func() []byte {
	ctx := context.Background()

	suDSN = os.Getenv("TEST_DATABASE_DSN")
	if suDSN == "" {
		Fail("TEST_DATABASE_DSN not set — run: task test:integration:db")
	}

	migrator, err := db.NewMigrator(suDSN)
	Expect(err).NotTo(HaveOccurred(), "failed to create migrator")
	Expect(migrator.Up(ctx)).To(Succeed(), "failed to run migrations")
	Expect(migrator.Close()).To(Succeed(), "failed to close migrator")

	adminDB, err := sql.Open("pgx", suDSN)
	Expect(err).NotTo(HaveOccurred(), "failed to open admin connection")
	_, err = adminDB.ExecContext(ctx, "ALTER ROLE app_user WITH PASSWORD 'apppass'")
	Expect(err).NotTo(HaveOccurred(), "failed to set app_user password")
	Expect(adminDB.Close()).To(Succeed(), "failed to close admin connection")

	return []byte(suDSN)
}, func(data []byte) {
	ctx := context.Background()
	suDSN = string(data)

	var err error
	suPool, err = db.NewPool(db.PoolConfig{
		DSN:          suDSN,
		MaxOpenConns: 5,
		Extensions:   []string{"age", "vector"},
	})
	Expect(err).NotTo(HaveOccurred(), "failed to create superuser pool")

	// Verify migration 010 applied (controls table must exist).
	err = suPool.Exec(ctx, "SELECT 1 FROM controls LIMIT 0")
	Expect(err).NotTo(HaveOccurred(),
		"controls table does not exist — migration 010 may not have applied.\n"+
			"If the schema_migrations table is dirty, run: task test:integration:clean")
})

var _ = SynchronizedAfterSuite(func() {
	// per-process cleanup (nothing needed)
}, func() {
	if suPool != nil {
		suPool.Close()
	}
})

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func appUserDSN() string {
	u, err := url.Parse(suDSN)
	Expect(err).NotTo(HaveOccurred(), "bad suDSN")
	u.User = url.UserPassword("app_user", "apppass")
	return u.String()
}

func appUserConn() *sql.DB {
	conn, err := sql.Open("pgx", appUserDSN())
	Expect(err).NotTo(HaveOccurred(), "failed to open app_user connection")
	DeferCleanup(func() { conn.Close() })
	return conn
}

func tenantConn() db.Connection {
	pool, err := db.NewPool(db.PoolConfig{
		DSN:          appUserDSN(),
		MaxOpenConns: 2,
	})
	Expect(err).NotTo(HaveOccurred(), "failed to create app_user pool")
	DeferCleanup(func() { pool.Close() })
	return db.NewTenantPool(pool)
}

func setupTenant(tenantID, displayName string) {
	ctx := context.Background()
	err := suPool.Exec(ctx,
		"INSERT INTO tenants (tenant_id, display_name) VALUES ($1, $2) ON CONFLICT DO NOTHING",
		tenantID, displayName)
	Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("setupTenant(%q)", tenantID))
}

func setupCatalog(conn *sql.DB, tenantID, catalogID, name string) {
	ctx := context.Background()
	tx, err := conn.BeginTx(ctx, nil)
	Expect(err).NotTo(HaveOccurred(), "BeginTx")
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, "SELECT set_config('app.current_tenant', $1, true)", tenantID)
	Expect(err).NotTo(HaveOccurred(), "set_config")
	_, err = tx.ExecContext(ctx,
		"INSERT INTO catalogs (catalog_id, tenant_id, name, version, source_type, object_path) VALUES ($1, $2, $3, '1.0', 'oscal', 'test') ON CONFLICT DO NOTHING",
		catalogID, tenantID, name)
	Expect(err).NotTo(HaveOccurred(), "insert catalog")
	Expect(tx.Commit()).To(Succeed(), "commit")
}

func execAsTenant(conn *sql.DB, tenantID string, fn func(tx *sql.Tx)) {
	ctx := context.Background()
	tx, err := conn.BeginTx(ctx, nil)
	Expect(err).NotTo(HaveOccurred(), "BeginTx")
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, "SELECT set_config('app.current_tenant', $1, true)", tenantID)
	Expect(err).NotTo(HaveOccurred(), "set_config tenant")
	fn(tx)
	Expect(tx.Commit()).To(Succeed(), "Commit")
}

// ---------------------------------------------------------------------------
// Store integration specs
// ---------------------------------------------------------------------------

var _ = Describe("PGStore Integration", func() {
	Describe("UpsertAndGetCatalog", func() {
		It("inserts and retrieves a catalog record via raw SQL", func() {
			setupTenant("cat-test-alpha", "Catalog Alpha")
			conn := tenantConn()
			rawConn := appUserConn()

			store := catalog.NewPGStore(conn)
			catalogID := fmt.Sprintf("cat-int-%d", time.Now().UnixNano())

			execAsTenant(rawConn, "cat-test-alpha", func(tx *sql.Tx) {
				_, err := tx.ExecContext(context.Background(),
					`INSERT INTO catalogs (catalog_id, tenant_id, name, version, source_type, object_path,
						source_uri, content_hash, content_size, format, output_hash, extractor_name, extractor_version)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
					catalogID, "cat-test-alpha", "Test Catalog", "1.0", "oscal", "/test/path",
					"https://example.com/catalog.json", "abc123hash", 12345,
					"oscal-json", "def456hash", "oscal-parser", "0.1.0")
				Expect(err).NotTo(HaveOccurred(), "insert catalog")
			})

			execAsTenant(rawConn, "cat-test-alpha", func(tx *sql.Tx) {
				var rec catalog.CatalogRecord
				err := tx.QueryRowContext(context.Background(), `
					SELECT catalog_id, tenant_id, name, version, source_type, object_path,
						COALESCE(source_uri, ''), COALESCE(content_hash, ''), COALESCE(content_size, 0),
						COALESCE(format, ''), COALESCE(output_hash, ''),
						COALESCE(extractor_name, ''), COALESCE(extractor_version, '')
					FROM catalogs WHERE catalog_id = $1`, catalogID).Scan(
					&rec.CatalogID, &rec.TenantID, &rec.Name, &rec.Version,
					&rec.SourceType, &rec.ObjectPath,
					&rec.SourceURI, &rec.ContentHash, &rec.ContentSize,
					&rec.Format, &rec.OutputHash,
					&rec.ExtractorName, &rec.ExtractorVersion)
				Expect(err).NotTo(HaveOccurred(), "get catalog")
				Expect(rec.CatalogID).To(Equal(catalogID))
				Expect(rec.Name).To(Equal("Test Catalog"))
				Expect(rec.ContentHash).To(Equal("abc123hash"))
				Expect(rec.ContentSize).To(Equal(int64(12345)))
				Expect(rec.ExtractorName).To(Equal("oscal-parser"))
			})

			_ = store // compile check
		})
	})

	Describe("UpsertControls", func() {
		It("inserts controls with JSONB props and verifies persistence", func() {
			tenantID := "ctrl-test-alpha"
			catalogID := fmt.Sprintf("ctrl-cat-%d", time.Now().UnixNano())

			setupTenant(tenantID, "Control Alpha")
			rawConn := appUserConn()
			setupCatalog(rawConn, tenantID, catalogID, "Control Test Catalog")

			controls := []catalog.ControlRecord{
				{
					TenantID: tenantID, ControlID: fmt.Sprintf("%s/ac-1", catalogID),
					CatalogID: catalogID, Identifier: "ac-1",
					Title: "Policy and Procedures", Statement: "Develop and maintain policy.",
					Class: "compliance-requirement", GroupID: "ac",
					Props: map[string]string{"source": "nist"}, CreatedAt: time.Now().UTC(),
				},
				{
					TenantID: tenantID, ControlID: fmt.Sprintf("%s/ac-2", catalogID),
					CatalogID: catalogID, Identifier: "ac-2",
					Title: "Account Management", Statement: "Manage information system accounts.",
					Class: "compliance-section", GroupID: "ac",
					Props: map[string]string{}, CreatedAt: time.Now().UTC(),
				},
				{
					TenantID: tenantID, ControlID: fmt.Sprintf("%s/ac-2.a", catalogID),
					CatalogID: catalogID, Identifier: "ac-2.a",
					Title: "", Statement: "Identify account types.",
					Class: "compliance-requirement", ParentID: "ac-2", GroupID: "ac",
					Props: map[string]string{"parent-id": "ac-2"}, CreatedAt: time.Now().UTC(),
				},
			}

			conn := tenantConn()
			store := catalog.NewPGStore(conn)
			ctx, err := tenant.WithTenant(context.Background(), tenantID)
			Expect(err).NotTo(HaveOccurred())
			Expect(store.UpsertControls(ctx, controls)).To(Succeed())

			execAsTenant(rawConn, tenantID, func(tx *sql.Tx) {
				var count int
				err := tx.QueryRowContext(context.Background(),
					"SELECT COUNT(*) FROM controls WHERE catalog_id = $1", catalogID).Scan(&count)
				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(Equal(3))
			})

			execAsTenant(rawConn, tenantID, func(tx *sql.Tx) {
				var propsJSON []byte
				err := tx.QueryRowContext(context.Background(),
					"SELECT props FROM controls WHERE control_id = $1",
					fmt.Sprintf("%s/ac-1", catalogID)).Scan(&propsJSON)
				Expect(err).NotTo(HaveOccurred())
				var props map[string]string
				Expect(json.Unmarshal(propsJSON, &props)).To(Succeed())
				Expect(props["source"]).To(Equal("nist"))
			})
		})

		It("updates existing controls via upsert", func() {
			tenantID := "upsert-test"
			catalogID := fmt.Sprintf("upsert-cat-%d", time.Now().UnixNano())

			setupTenant(tenantID, "Upsert Test")
			rawConn := appUserConn()
			setupCatalog(rawConn, tenantID, catalogID, "Upsert Catalog")

			conn := tenantConn()
			store := catalog.NewPGStore(conn)
			ctx, err := tenant.WithTenant(context.Background(), tenantID)
			Expect(err).NotTo(HaveOccurred())
			controlID := fmt.Sprintf("%s/ac-1", catalogID)

			initial := []catalog.ControlRecord{{
				TenantID: tenantID, ControlID: controlID,
				CatalogID: catalogID, Identifier: "ac-1",
				Title: "Original Title", Statement: "Original statement.",
				Class: "compliance-requirement", Props: map[string]string{},
				CreatedAt: time.Now().UTC(),
			}}
			Expect(store.UpsertControls(ctx, initial)).To(Succeed())

			updated := []catalog.ControlRecord{{
				TenantID: tenantID, ControlID: controlID,
				CatalogID: catalogID, Identifier: "ac-1",
				Title: "Updated Title", Statement: "Updated statement.",
				Class: "compliance-requirement", Props: map[string]string{"version": "2"},
				CreatedAt: time.Now().UTC(),
			}}
			Expect(store.UpsertControls(ctx, updated)).To(Succeed())

			execAsTenant(rawConn, tenantID, func(tx *sql.Tx) {
				var title, statement string
				err := tx.QueryRowContext(context.Background(),
					"SELECT title, statement FROM controls WHERE control_id = $1", controlID).Scan(&title, &statement)
				Expect(err).NotTo(HaveOccurred())
				Expect(title).To(Equal("Updated Title"))
				Expect(statement).To(Equal("Updated statement."))
			})

			execAsTenant(rawConn, tenantID, func(tx *sql.Tx) {
				var count int
				err := tx.QueryRowContext(context.Background(),
					"SELECT COUNT(*) FROM controls WHERE control_id = $1", controlID).Scan(&count)
				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(Equal(1), "upsert should update, not duplicate")
			})
		})
	})

	Describe("FullTextSearch", func() {
		var (
			tenantID  string
			catalogID string
			rawConn   *sql.DB
		)

		BeforeEach(func() {
			tenantID = "fts-test"
			catalogID = fmt.Sprintf("fts-cat-%d", time.Now().UnixNano())
			setupTenant(tenantID, "FTS Test")
			rawConn = appUserConn()
			setupCatalog(rawConn, tenantID, catalogID, "FTS Catalog")

			controls := []catalog.ControlRecord{
				{
					TenantID: tenantID, ControlID: fmt.Sprintf("%s/ac-1", catalogID),
					CatalogID: catalogID, Identifier: "ac-1",
					Title:     "Access Control Policy",
					Statement: "Develop, document, and disseminate an access control policy.",
					Class:     "compliance-requirement", Props: map[string]string{},
					CreatedAt: time.Now().UTC(),
				},
				{
					TenantID: tenantID, ControlID: fmt.Sprintf("%s/au-1", catalogID),
					CatalogID: catalogID, Identifier: "au-1",
					Title:     "Audit Policy",
					Statement: "Develop, document, and disseminate an audit and accountability policy.",
					Class:     "compliance-requirement", Props: map[string]string{},
					CreatedAt: time.Now().UTC(),
				},
				{
					TenantID: tenantID, ControlID: fmt.Sprintf("%s/si-1", catalogID),
					CatalogID: catalogID, Identifier: "si-1",
					Title:     "System and Information Integrity Policy",
					Statement: "Develop system integrity procedures for malicious code protection.",
					Class:     "compliance-requirement", Props: map[string]string{},
					CreatedAt: time.Now().UTC(),
				},
			}

			conn := tenantConn()
			store := catalog.NewPGStore(conn)
			ctx, err := tenant.WithTenant(context.Background(), tenantID)
			Expect(err).NotTo(HaveOccurred())
			Expect(store.UpsertControls(ctx, controls)).To(Succeed())
		})

		It("ranks 'access control' with ac-1 highest", func() {
			execAsTenant(rawConn, tenantID, func(tx *sql.Tx) {
				rows, err := tx.QueryContext(context.Background(), `
					SELECT control_id, ts_rank_cd(search_vector, plainto_tsquery('english', $1)) AS rank
					FROM controls
					WHERE search_vector @@ plainto_tsquery('english', $1)
					  AND catalog_id = $2
					ORDER BY rank DESC`, "access control", catalogID)
				Expect(err).NotTo(HaveOccurred())
				defer rows.Close()

				type result struct {
					ControlID string
					Rank      float64
				}
				var results []result
				for rows.Next() {
					var r result
					Expect(rows.Scan(&r.ControlID, &r.Rank)).To(Succeed())
					results = append(results, r)
				}
				Expect(rows.Err()).NotTo(HaveOccurred())
				Expect(results).NotTo(BeEmpty(), "FTS returned 0 results for 'access control'")
				Expect(results[0].ControlID).To(HaveSuffix("/ac-1"))
			})
		})

		It("matches 'malicious code' to si-1 only", func() {
			execAsTenant(rawConn, tenantID, func(tx *sql.Tx) {
				var count int
				err := tx.QueryRowContext(context.Background(), `
					SELECT COUNT(*) FROM controls
					WHERE search_vector @@ plainto_tsquery('english', $1)
					  AND catalog_id = $2`, "malicious code", catalogID).Scan(&count)
				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(Equal(1))
			})
		})

		It("returns 0 results for nonexistent term", func() {
			execAsTenant(rawConn, tenantID, func(tx *sql.Tx) {
				var count int
				err := tx.QueryRowContext(context.Background(), `
					SELECT COUNT(*) FROM controls
					WHERE search_vector @@ plainto_tsquery('english', $1)
					  AND catalog_id = $2`, "xyznonexistent", catalogID).Scan(&count)
				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(Equal(0))
			})
		})
	})

	Describe("TenantIsolation", func() {
		It("enforces RLS so tenants cannot see each other's controls", func() {
			tenantAlpha := "cat-iso-alpha"
			tenantBeta := "cat-iso-beta"
			catalogAlpha := fmt.Sprintf("iso-cat-alpha-%d", time.Now().UnixNano())
			catalogBeta := fmt.Sprintf("iso-cat-beta-%d", time.Now().UnixNano())

			setupTenant(tenantAlpha, "Alpha Corp")
			setupTenant(tenantBeta, "Beta Inc")
			conn := appUserConn()
			setupCatalog(conn, tenantAlpha, catalogAlpha, "Alpha Catalog")
			setupCatalog(conn, tenantBeta, catalogBeta, "Beta Catalog")

			execAsTenant(conn, tenantAlpha, func(tx *sql.Tx) {
				_, err := tx.ExecContext(context.Background(), `
					INSERT INTO controls (tenant_id, control_id, catalog_id, identifier, title, statement, class)
					VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT DO NOTHING`,
					tenantAlpha, catalogAlpha+"/ac-1", catalogAlpha, "ac-1",
					"Alpha Control", "Alpha policy.", "compliance-requirement")
				Expect(err).NotTo(HaveOccurred())
			})

			execAsTenant(conn, tenantBeta, func(tx *sql.Tx) {
				_, err := tx.ExecContext(context.Background(), `
					INSERT INTO controls (tenant_id, control_id, catalog_id, identifier, title, statement, class)
					VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT DO NOTHING`,
					tenantBeta, catalogBeta+"/si-1", catalogBeta, "si-1",
					"Beta Control", "Beta integrity.", "compliance-requirement")
				Expect(err).NotTo(HaveOccurred())
			})

			// Alpha should only see alpha's controls
			execAsTenant(conn, tenantAlpha, func(tx *sql.Tx) {
				rows, err := tx.QueryContext(context.Background(), "SELECT control_id, tenant_id FROM controls")
				Expect(err).NotTo(HaveOccurred())
				defer rows.Close()

				for rows.Next() {
					var id, tid string
					Expect(rows.Scan(&id, &tid)).To(Succeed())
					Expect(tid).To(Equal(tenantAlpha), "alpha session sees non-alpha tenant_id")
					Expect(id).NotTo(ContainSubstring("beta"))
					Expect(id).NotTo(ContainSubstring(tenantBeta))
				}
				Expect(rows.Err()).NotTo(HaveOccurred())
			})

			// Beta should only see beta's controls
			execAsTenant(conn, tenantBeta, func(tx *sql.Tx) {
				rows, err := tx.QueryContext(context.Background(), "SELECT control_id FROM controls")
				Expect(err).NotTo(HaveOccurred())
				defer rows.Close()

				for rows.Next() {
					var id string
					Expect(rows.Scan(&id)).To(Succeed())
					Expect(id).NotTo(ContainSubstring("alpha"))
					Expect(id).NotTo(ContainSubstring(tenantAlpha))
				}
				Expect(rows.Err()).NotTo(HaveOccurred())
			})

			// No tenant context: should see zero controls (RLS fail-closed)
			var count int
			err := conn.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM controls").Scan(&count)
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(Equal(0), "no-context query returned controls (RLS fail-closed)")
		})
	})

	Describe("SearchViaStore", func() {
		It("returns results through the Store interface with tenant context", func() {
			tenantID := "store-search-test"
			catalogID := fmt.Sprintf("store-search-cat-%d", time.Now().UnixNano())

			setupTenant(tenantID, "Store Search")
			rawConn := appUserConn()
			setupCatalog(rawConn, tenantID, catalogID, "Search Catalog")

			conn := tenantConn()
			store := catalog.NewPGStore(conn)
			ctx, err := tenant.WithTenant(context.Background(), tenantID)
			Expect(err).NotTo(HaveOccurred())

			controls := []catalog.ControlRecord{
				{
					TenantID: tenantID, ControlID: fmt.Sprintf("%s/ac-1", catalogID),
					CatalogID: catalogID, Identifier: "ac-1",
					Title:     "Access Control Policy",
					Statement: "Develop and disseminate an access control policy.",
					Class:     "compliance-requirement", Props: map[string]string{},
					CreatedAt: time.Now().UTC(),
				},
				{
					TenantID: tenantID, ControlID: fmt.Sprintf("%s/au-1", catalogID),
					CatalogID: catalogID, Identifier: "au-1",
					Title:     "Audit Policy",
					Statement: "Maintain audit logs for all system events.",
					Class:     "compliance-requirement", Props: map[string]string{},
					CreatedAt: time.Now().UTC(),
				},
			}

			Expect(store.UpsertControls(ctx, controls)).To(Succeed())

			results, pageInfo, err := store.SearchControls(ctx, catalog.SearchQuery{
				Query: "access control policy",
				Limit: 10,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(results).NotTo(BeEmpty())

			found := false
			for _, r := range results {
				if r.Identifier == "ac-1" {
					found = true
				}
			}
			Expect(found).To(BeTrue(), "ac-1 not found in search results")
			Expect(pageInfo.TotalCount).To(BeNumerically(">=", 1))
		})
	})

	Describe("CatalogProvenanceColumns", func() {
		It("stores and retrieves provenance metadata", func() {
			tenantID := "prov-test"
			catalogID := fmt.Sprintf("prov-cat-%d", time.Now().UnixNano())

			setupTenant(tenantID, "Provenance Test")
			conn := appUserConn()

			execAsTenant(conn, tenantID, func(tx *sql.Tx) {
				_, err := tx.ExecContext(context.Background(), `
					INSERT INTO catalogs (catalog_id, tenant_id, name, version, source_type, object_path,
						source_uri, content_hash, content_size, format, output_hash, extractor_name, extractor_version)
					VALUES ($1, $2, 'Provenance Catalog', '1.0', 'oscal', '/test',
						'https://nist.gov/sp800-53', 'sha256abcdef', 42000,
						'oscal-json', 'sha256output', 'oscal-parser', '0.2.0')`,
					catalogID, tenantID)
				Expect(err).NotTo(HaveOccurred())
			})

			execAsTenant(conn, tenantID, func(tx *sql.Tx) {
				var sourceURI, contentHash, format, outputHash, extractorName, extractorVersion string
				var contentSize int64
				err := tx.QueryRowContext(context.Background(), `
					SELECT source_uri, content_hash, content_size, format, output_hash, extractor_name, extractor_version
					FROM catalogs WHERE catalog_id = $1`, catalogID).Scan(
					&sourceURI, &contentHash, &contentSize, &format, &outputHash, &extractorName, &extractorVersion)
				Expect(err).NotTo(HaveOccurred())
				Expect(sourceURI).To(Equal("https://nist.gov/sp800-53"))
				Expect(contentHash).To(Equal("sha256abcdef"))
				Expect(contentSize).To(Equal(int64(42000)))
				Expect(format).To(Equal("oscal-json"))
				Expect(extractorName).To(Equal("oscal-parser"))
			})
		})
	})

	// --- Store-abstraction tests ---

	Describe("UpsertCatalogViaStore", func() {
		It("creates and updates catalog records through the Store interface", func() {
			tenantID := "store-upsert-cat"
			setupTenant(tenantID, "Store Upsert Catalog")
			conn := tenantConn()
			store := catalog.NewPGStore(conn)
			ctx, err := tenant.WithTenant(context.Background(), tenantID)
			Expect(err).NotTo(HaveOccurred())
			catalogID := fmt.Sprintf("store-cat-%d", time.Now().UnixNano())

			rec := catalog.CatalogRecord{
				CatalogID:        catalogID,
				TenantID:         tenantID,
				Name:             "Store Test Catalog",
				Version:          "2.0",
				SourceType:       "oscal",
				ObjectPath:       "/store/test",
				CreatedAt:        time.Now().UTC(),
				SourceURI:        "https://example.com/store",
				ContentHash:      "storehash123",
				ContentSize:      99999,
				Format:           "oscal-json",
				OutputHash:       "outputhash456",
				ExtractorName:    "store-parser",
				ExtractorVersion: "1.0.0",
			}

			Expect(store.UpsertCatalog(ctx, rec)).To(Succeed())

			got, err := store.GetCatalog(ctx, catalogID)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Name).To(Equal("Store Test Catalog"))
			Expect(got.Version).To(Equal("2.0"))
			Expect(got.SourceURI).To(Equal("https://example.com/store"))
			Expect(got.ContentSize).To(Equal(int64(99999)))
			Expect(got.ExtractorName).To(Equal("store-parser"))

			// Upsert again with updated name — should update, not create new
			rec.Name = "Updated Catalog Name"
			Expect(store.UpsertCatalog(ctx, rec)).To(Succeed())

			got, err = store.GetCatalog(ctx, catalogID)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Name).To(Equal("Updated Catalog Name"))
		})
	})

	Describe("GetCatalog_NotFound", func() {
		It("returns an error for a nonexistent catalog", func() {
			tenantID := "store-notfound"
			setupTenant(tenantID, "Not Found Test")
			conn := tenantConn()
			store := catalog.NewPGStore(conn)
			ctx, err := tenant.WithTenant(context.Background(), tenantID)
			Expect(err).NotTo(HaveOccurred())

			_, err = store.GetCatalog(ctx, "nonexistent-catalog-id")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not found"))
		})
	})

	Describe("ListCatalogs", func() {
		It("paginates catalog records", func() {
			tenantID := "store-list-test"
			setupTenant(tenantID, "List Test")
			conn := tenantConn()
			store := catalog.NewPGStore(conn)
			ctx, err := tenant.WithTenant(context.Background(), tenantID)
			Expect(err).NotTo(HaveOccurred())

			// Insert 3 catalogs
			for i := 0; i < 3; i++ {
				catID := fmt.Sprintf("list-cat-%d-%d", time.Now().UnixNano(), i)
				rec := catalog.CatalogRecord{
					CatalogID:  catID,
					TenantID:   tenantID,
					Name:       fmt.Sprintf("Catalog %d", i),
					Version:    "1.0",
					SourceType: "oscal",
					ObjectPath: "/list/test",
					CreatedAt:  time.Now().UTC(),
				}
				Expect(store.UpsertCatalog(ctx, rec)).To(Succeed())
			}

			records, pageInfo, err := store.ListCatalogs(ctx, catalog.ListOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(len(records)).To(BeNumerically(">=", 3))
			Expect(pageInfo.TotalCount).To(BeNumerically(">=", 3))

			records, pageInfo, err = store.ListCatalogs(ctx, catalog.ListOptions{Limit: 1})
			Expect(err).NotTo(HaveOccurred())
			Expect(records).To(HaveLen(1))
			Expect(pageInfo.NextOffset).To(Equal(1))
		})
	})

	Describe("GetControlViaStore", func() {
		It("retrieves control details including props", func() {
			tenantID := "store-getctrl"
			catalogID := fmt.Sprintf("getctrl-cat-%d", time.Now().UnixNano())
			setupTenant(tenantID, "Get Control")
			rawConn := appUserConn()
			setupCatalog(rawConn, tenantID, catalogID, "GetCtrl Catalog")
			conn := tenantConn()
			store := catalog.NewPGStore(conn)
			ctx, err := tenant.WithTenant(context.Background(), tenantID)
			Expect(err).NotTo(HaveOccurred())

			controlID := fmt.Sprintf("%s/ac-1", catalogID)

			Expect(store.UpsertControls(ctx, []catalog.ControlRecord{{
				TenantID:   tenantID,
				ControlID:  controlID,
				CatalogID:  catalogID,
				Identifier: "ac-1",
				Title:      "Access Control Policy",
				Statement:  "Develop policy.",
				Class:      "compliance-requirement",
				ParentID:   "",
				GroupID:    "ac",
				Props:      map[string]string{"framework": "nist"},
				CreatedAt:  time.Now().UTC(),
			}})).To(Succeed())

			got, err := store.GetControl(ctx, controlID)
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Identifier).To(Equal("ac-1"))
			Expect(got.Title).To(Equal("Access Control Policy"))
			Expect(got.Class).To(Equal("compliance-requirement"))
			Expect(got.GroupID).To(Equal("ac"))
			Expect(got.Props["framework"]).To(Equal("nist"))

			// GetControl for nonexistent
			_, err = store.GetControl(ctx, "nonexistent")
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("FTSWeightedRanking", func() {
		It("ranks title matches higher than body matches", func() {
			tenantID := "fts-weight-test"
			catalogID := fmt.Sprintf("fts-weight-%d", time.Now().UnixNano())

			setupTenant(tenantID, "FTS Weight")
			rawConn := appUserConn()
			setupCatalog(rawConn, tenantID, catalogID, "Weight Catalog")

			controls := []catalog.ControlRecord{
				{
					TenantID: tenantID, ControlID: fmt.Sprintf("%s/title-match", catalogID),
					CatalogID: catalogID, Identifier: "title-match",
					Title:     "Encryption Policy",
					Statement: "Implement data protection measures.",
					Class:     "compliance-requirement", Props: map[string]string{},
					CreatedAt: time.Now().UTC(),
				},
				{
					TenantID: tenantID, ControlID: fmt.Sprintf("%s/body-match", catalogID),
					CatalogID: catalogID, Identifier: "body-match",
					Title:     "Data Protection",
					Statement: "Apply encryption to all data at rest and in transit.",
					Class:     "compliance-requirement", Props: map[string]string{},
					CreatedAt: time.Now().UTC(),
				},
			}

			conn := tenantConn()
			store := catalog.NewPGStore(conn)
			ctx, err := tenant.WithTenant(context.Background(), tenantID)
			Expect(err).NotTo(HaveOccurred())
			Expect(store.UpsertControls(ctx, controls)).To(Succeed())

			execAsTenant(rawConn, tenantID, func(tx *sql.Tx) {
				rows, err := tx.QueryContext(context.Background(), `
					SELECT identifier, ts_rank_cd(search_vector, plainto_tsquery('english', $1)) AS rank
					FROM controls
					WHERE catalog_id = $2 AND search_vector @@ plainto_tsquery('english', $1)
					ORDER BY rank DESC`, "encryption", catalogID)
				Expect(err).NotTo(HaveOccurred())
				defer rows.Close()

				type result struct {
					Identifier string
					Rank       float64
				}
				var results []result
				for rows.Next() {
					var r result
					Expect(rows.Scan(&r.Identifier, &r.Rank)).To(Succeed())
					results = append(results, r)
				}

				Expect(results).To(HaveLen(2))
				Expect(results[0].Identifier).To(Equal("title-match"),
					fmt.Sprintf("expected title-match to rank first, got %q (%.4f) before %q (%.4f)",
						results[0].Identifier, results[0].Rank,
						results[1].Identifier, results[1].Rank))
			})
		})
	})
})

// ---------------------------------------------------------------------------
// End-to-end specs
// ---------------------------------------------------------------------------

var _ = Describe("E2E Integration", func() {
	Describe("ParseCatalogAndSearch", func() {
		It("parses OSCAL fixture through real parser, persists, and searches", func() {
			tenantID := "e2e-oscal"
			setupTenant(tenantID, "E2E OSCAL")
			rawConn := appUserConn()

			fixtureData, err := os.ReadFile("../../pkg/oscal/testdata/minimal_catalog.json")
			Expect(err).NotTo(HaveOccurred(), "read fixture")

			parser := oscal.NewParser("")
			items, err := parser.Parse(context.Background(), bytes.NewReader(fixtureData))
			Expect(err).NotTo(HaveOccurred())
			Expect(items).NotTo(BeEmpty(), "parser returned 0 items")

			// Verify parser produced expected controls
			ids := make(map[string]bool)
			for _, item := range items {
				ids[item.ID] = true
			}
			for _, want := range []string{"ac-1", "ac-2", "ac-3", "ac-2.1"} {
				Expect(ids).To(HaveKey(want), fmt.Sprintf("expected control %q in parsed output", want))
			}

			// Verify AC-2 was decomposed (parent + children)
			var ac2Parent *oscal.ControlItem
			var ac2Children int
			for i := range items {
				if items[i].ID == "ac-2" {
					ac2Parent = &items[i]
				}
				if items[i].ParentID == "ac-2" {
					ac2Children++
				}
			}
			Expect(ac2Parent).NotTo(BeNil(), "ac-2 not found in parsed output")
			Expect(ac2Parent.Class).To(Equal(oscal.ClassSection),
				"decomposed parent should be section")
			Expect(ac2Children).To(BeNumerically(">", 0),
				"ac-2 has no children — decomposition did not produce child items")

			// Verify parameter substitution in AC-1
			ac1, err := parser.FindControl(items, "ac-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(ac1.Text).NotTo(ContainSubstring("{{ insert:"),
				"ac-1 still has unresolved template")
			Expect(ac1.Text).To(ContainSubstring("organization-defined personnel or roles"))

			// Persist: create catalog, then upsert controls via Store
			catalogID := fmt.Sprintf("e2e-cat-%d", time.Now().UnixNano())
			conn := tenantConn()
			store := catalog.NewPGStore(conn)
			ctx, err := tenant.WithTenant(context.Background(), tenantID)
			Expect(err).NotTo(HaveOccurred())

			catRec := catalog.CatalogRecord{
				CatalogID:     catalogID,
				TenantID:      tenantID,
				Name:          "E2E Test Catalog (NIST SP 800-53 subset)",
				Version:       "1.0",
				SourceType:    "oscal-json",
				ObjectPath:    "testdata/minimal_catalog.json",
				CreatedAt:     time.Now().UTC(),
				Format:        "oscal-json",
				ContentHash:   "e2e-fixture-hash",
				ExtractorName: "oscal-parser",
			}
			Expect(store.UpsertCatalog(ctx, catRec)).To(Succeed())

			controlRecords := make([]catalog.ControlRecord, len(items))
			for i, item := range items {
				controlRecords[i] = catalog.ControlRecord{
					TenantID:   tenantID,
					ControlID:  fmt.Sprintf("%s/%s", catalogID, item.ID),
					CatalogID:  catalogID,
					Identifier: item.ID,
					Title:      item.Title,
					Statement:  item.Text,
					Class:      item.Class,
					ParentID:   item.ParentID,
					GroupID:    item.GroupID,
					Props:      item.Props,
					CreatedAt:  time.Now().UTC(),
				}
			}
			Expect(store.UpsertControls(ctx, controlRecords)).To(Succeed())

			// Verify all controls persisted
			execAsTenant(rawConn, tenantID, func(tx *sql.Tx) {
				var count int
				err := tx.QueryRowContext(context.Background(),
					"SELECT COUNT(*) FROM controls WHERE catalog_id = $1", catalogID).Scan(&count)
				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(Equal(len(items)))
			})

			// Full-text search: "account management" should find AC-2 controls
			By("searching for 'account management'")
			results, _, err := store.SearchControls(ctx, catalog.SearchQuery{
				Query:      "account management",
				CatalogIDs: []string{catalogID},
				Limit:      20,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(results).NotTo(BeEmpty())
			foundAC2 := false
			for _, r := range results {
				if r.Identifier == "ac-2" || strings.HasPrefix(r.Identifier, "ac-2.") {
					foundAC2 = true
				}
			}
			Expect(foundAC2).To(BeTrue(), "search 'account management' did not find ac-2")

			// Full-text search: "access enforcement" should find AC-3
			By("searching for 'access enforcement'")
			results, _, err = store.SearchControls(ctx, catalog.SearchQuery{
				Query:      "access enforcement",
				CatalogIDs: []string{catalogID},
				Limit:      20,
			})
			Expect(err).NotTo(HaveOccurred())
			foundAC3 := false
			for _, r := range results {
				if r.Identifier == "ac-3" {
					foundAC3 = true
				}
			}
			Expect(foundAC3).To(BeTrue(), "search 'access enforcement' did not find ac-3")

			// Full-text search: "automated mechanism" should find AC-2.1
			By("searching for 'automated mechanisms'")
			results, _, err = store.SearchControls(ctx, catalog.SearchQuery{
				Query:      "automated mechanisms",
				CatalogIDs: []string{catalogID},
				Limit:      20,
			})
			Expect(err).NotTo(HaveOccurred())
			foundAC21 := false
			for _, r := range results {
				if r.Identifier == "ac-2.1" {
					foundAC21 = true
				}
			}
			Expect(foundAC21).To(BeTrue(), "search 'automated mechanisms' did not find ac-2.1")

			// Negative: unrelated term returns no results
			By("searching for 'blockchain cryptocurrency'")
			results, _, err = store.SearchControls(ctx, catalog.SearchQuery{
				Query:      "blockchain cryptocurrency",
				CatalogIDs: []string{catalogID},
				Limit:      20,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(BeEmpty())

			// Re-import: upsert the same controls with updated titles
			By("re-importing with updated titles")
			for i := range controlRecords {
				controlRecords[i].Title = controlRecords[i].Title + " (updated)"
			}
			Expect(store.UpsertControls(ctx, controlRecords)).To(Succeed())

			// Verify count didn't change (upsert, not duplicate)
			execAsTenant(rawConn, tenantID, func(tx *sql.Tx) {
				var count int
				err := tx.QueryRowContext(context.Background(),
					"SELECT COUNT(*) FROM controls WHERE catalog_id = $1", catalogID).Scan(&count)
				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(Equal(len(items)), "upsert should not duplicate")
			})

			// Verify title was updated
			got, err := store.GetControl(ctx, fmt.Sprintf("%s/ac-3", catalogID))
			Expect(err).NotTo(HaveOccurred())
			Expect(got.Title).To(HaveSuffix("(updated)"))

			// Tenant isolation: different tenant cannot see these controls
			otherTenant := "e2e-other"
			setupTenant(otherTenant, "Other Tenant")
			execAsTenant(rawConn, otherTenant, func(tx *sql.Tx) {
				var count int
				err := tx.QueryRowContext(context.Background(),
					"SELECT COUNT(*) FROM controls WHERE catalog_id = $1", catalogID).Scan(&count)
				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(Equal(0), "other tenant sees controls from e2e catalog")
			})
		})
	})

	Describe("ParserRejectsInvalidOSCAL", func() {
		var parser oscal.Parser

		BeforeEach(func() {
			parser = oscal.NewParser("")
		})

		It("rejects malformed JSON", func() {
			_, err := parser.Parse(context.Background(), bytes.NewReader([]byte(`{not json`)))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(SatisfyAny(
				ContainSubstring("parse"),
				ContainSubstring("unmarshal"),
			))
		})

		It("rejects valid JSON but not a catalog", func() {
			_, err := parser.Parse(context.Background(), bytes.NewReader([]byte(`{"profile":{"uuid":"x"}}`)))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("format"))
		})

		It("rejects catalog with no groups or controls", func() {
			emptyDoc := `{
				"catalog": {
					"uuid": "empty-test",
					"metadata": {
						"title": "Empty",
						"version": "1.0",
						"oscal-version": "1.1.3",
						"last-modified": "2026-01-01T00:00:00Z"
					}
				}
			}`
			_, err := parser.Parse(context.Background(), bytes.NewReader([]byte(emptyDoc)))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(SatisfyAny(
				ContainSubstring("no controls"),
				ContainSubstring("NoControls"),
			))
		})

		It("rejects empty input", func() {
			_, err := parser.Parse(context.Background(), bytes.NewReader([]byte{}))
			Expect(err).To(HaveOccurred())
		})

		It("rejects valid JSON object with null catalog", func() {
			_, err := parser.Parse(context.Background(), bytes.NewReader([]byte(`{"catalog": null}`)))
			Expect(err).To(HaveOccurred())
		})

		It("rejects array instead of object", func() {
			_, err := parser.Parse(context.Background(), bytes.NewReader([]byte(`[1,2,3]`)))
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("UpsertSubstantiveChanges", func() {
		It("handles re-import with substantive changes correctly", func() {
			tenantID := "e2e-upsert"
			setupTenant(tenantID, "E2E Upsert")
			rawConn := appUserConn()
			catalogID := fmt.Sprintf("e2e-upsert-cat-%d", time.Now().UnixNano())
			setupCatalog(rawConn, tenantID, catalogID, "Upsert E2E Catalog")
			conn := tenantConn()
			store := catalog.NewPGStore(conn)
			ctx, err := tenant.WithTenant(context.Background(), tenantID)
			Expect(err).NotTo(HaveOccurred())

			// Initial import: 3 controls, AC-2 is a section with one child
			initial := []catalog.ControlRecord{
				{
					TenantID: tenantID, ControlID: catalogID + "/ac-1", CatalogID: catalogID,
					Identifier: "ac-1", Title: "Policy and Procedures",
					Statement: "Develop access control policy.",
					Class:     "compliance-requirement", GroupID: "ac",
					Props: map[string]string{}, CreatedAt: time.Now().UTC(),
				},
				{
					TenantID: tenantID, ControlID: catalogID + "/ac-2", CatalogID: catalogID,
					Identifier: "ac-2", Title: "Account Management",
					Statement: "Manage accounts.",
					Class:     "compliance-section", GroupID: "ac",
					Props: map[string]string{}, CreatedAt: time.Now().UTC(),
				},
				{
					TenantID: tenantID, ControlID: catalogID + "/ac-2.a", CatalogID: catalogID,
					Identifier: "ac-2.a", Title: "",
					Statement: "Identify account types.",
					Class:     "compliance-requirement", ParentID: "ac-2", GroupID: "ac",
					Props: map[string]string{"parent-id": "ac-2"}, CreatedAt: time.Now().UTC(),
				},
			}
			Expect(store.UpsertControls(ctx, initial)).To(Succeed())

			// Snapshot initial state
			var initialAC1Stmt, initialAC2Class string
			var initialCreatedAt time.Time
			execAsTenant(rawConn, tenantID, func(tx *sql.Tx) {
				err := tx.QueryRowContext(context.Background(),
					"SELECT statement, created_at FROM controls WHERE control_id = $1",
					catalogID+"/ac-1").Scan(&initialAC1Stmt, &initialCreatedAt)
				Expect(err).NotTo(HaveOccurred())
				err = tx.QueryRowContext(context.Background(),
					"SELECT class FROM controls WHERE control_id = $1",
					catalogID+"/ac-2").Scan(&initialAC2Class)
				Expect(err).NotTo(HaveOccurred())
			})
			Expect(initialAC1Stmt).To(Equal("Develop access control policy."))
			Expect(initialAC2Class).To(Equal("compliance-section"))

			// Re-import with substantive changes
			reimport := []catalog.ControlRecord{
				{
					TenantID: tenantID, ControlID: catalogID + "/ac-1", CatalogID: catalogID,
					Identifier: "ac-1", Title: "Policy and Procedures",
					Statement: "Develop, document, and disseminate an access control policy that addresses purpose, scope, and roles.",
					Class:     "compliance-requirement", GroupID: "ac",
					Props: map[string]string{}, CreatedAt: time.Now().UTC(),
				},
				{
					TenantID: tenantID, ControlID: catalogID + "/ac-2", CatalogID: catalogID,
					Identifier: "ac-2", Title: "Account Management",
					Statement: "Manage information system accounts.",
					Class:     "compliance-requirement", GroupID: "ac",
					Props: map[string]string{}, CreatedAt: time.Now().UTC(),
				},
				{
					TenantID: tenantID, ControlID: catalogID + "/ac-2.a", CatalogID: catalogID,
					Identifier: "ac-2.a", Title: "",
					Statement: "Identify and select account types to support organizational missions and business functions.",
					Class:     "compliance-requirement", ParentID: "ac-2", GroupID: "ac",
					Props: map[string]string{"parent-id": "ac-2"}, CreatedAt: time.Now().UTC(),
				},
				{
					TenantID: tenantID, ControlID: catalogID + "/ac-2.b", CatalogID: catalogID,
					Identifier: "ac-2.b", Title: "",
					Statement: "Assign account managers for information system accounts.",
					Class:     "compliance-requirement", ParentID: "ac-2", GroupID: "ac",
					Props: map[string]string{"parent-id": "ac-2"}, CreatedAt: time.Now().UTC(),
				},
			}
			Expect(store.UpsertControls(ctx, reimport)).To(Succeed())

			// Verify: AC-1 statement text changed
			By("checking AC-1 statement was updated")
			execAsTenant(rawConn, tenantID, func(tx *sql.Tx) {
				var stmt string
				err := tx.QueryRowContext(context.Background(),
					"SELECT statement FROM controls WHERE control_id = $1",
					catalogID+"/ac-1").Scan(&stmt)
				Expect(err).NotTo(HaveOccurred())
				Expect(stmt).To(ContainSubstring("purpose, scope, and roles"))
			})

			// Verify: AC-2 class changed from section to requirement
			By("checking AC-2 class was re-classified")
			execAsTenant(rawConn, tenantID, func(tx *sql.Tx) {
				var class string
				err := tx.QueryRowContext(context.Background(),
					"SELECT class FROM controls WHERE control_id = $1",
					catalogID+"/ac-2").Scan(&class)
				Expect(err).NotTo(HaveOccurred())
				Expect(class).To(Equal("compliance-requirement"))
			})

			// Verify: AC-2.b was added (new control in re-import)
			By("checking AC-2.b was added")
			execAsTenant(rawConn, tenantID, func(tx *sql.Tx) {
				var stmt string
				err := tx.QueryRowContext(context.Background(),
					"SELECT statement FROM controls WHERE control_id = $1",
					catalogID+"/ac-2.b").Scan(&stmt)
				Expect(err).NotTo(HaveOccurred())
				Expect(stmt).To(ContainSubstring("account managers"))
			})

			// Verify: total count is now 4 (3 original + 1 new, not 7 duplicates)
			By("checking total control count")
			execAsTenant(rawConn, tenantID, func(tx *sql.Tx) {
				var count int
				err := tx.QueryRowContext(context.Background(),
					"SELECT COUNT(*) FROM controls WHERE catalog_id = $1", catalogID).Scan(&count)
				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(Equal(4), "3 upserted + 1 new")
			})

			// Verify: created_at was NOT changed on upserted controls
			By("checking created_at was preserved")
			execAsTenant(rawConn, tenantID, func(tx *sql.Tx) {
				var createdAt time.Time
				err := tx.QueryRowContext(context.Background(),
					"SELECT created_at FROM controls WHERE control_id = $1",
					catalogID+"/ac-1").Scan(&createdAt)
				Expect(err).NotTo(HaveOccurred())
				Expect(createdAt).To(BeTemporally("==", initialCreatedAt),
					"upsert should preserve created_at")
			})

			// Verify: FTS indexes updated (new text is searchable)
			By("checking FTS indexes were updated after upsert")
			execAsTenant(rawConn, tenantID, func(tx *sql.Tx) {
				var count int
				err := tx.QueryRowContext(context.Background(), `
					SELECT COUNT(*) FROM controls
					WHERE catalog_id = $1 AND search_vector @@ plainto_tsquery('english', $2)`,
					catalogID, "purpose scope roles").Scan(&count)
				Expect(err).NotTo(HaveOccurred())
				Expect(count).NotTo(Equal(0), "tsvector not updated after upsert")
			})

			// Verify: new control AC-2.b is searchable
			By("checking new control AC-2.b is searchable")
			results, _, err := store.SearchControls(ctx, catalog.SearchQuery{
				Query:      "account managers",
				CatalogIDs: []string{catalogID},
				Limit:      10,
			})
			Expect(err).NotTo(HaveOccurred())
			found := false
			for _, r := range results {
				if r.Identifier == "ac-2.b" {
					found = true
				}
			}
			Expect(found).To(BeTrue(), "newly added ac-2.b not found via FTS 'account managers'")
		})
	})
})

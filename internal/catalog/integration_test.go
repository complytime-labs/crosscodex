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

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/complytime-labs/crosscodex/internal/catalog"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/oscal"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

var suDSN string
var suPool db.Pool

func TestMain(m *testing.M) {
	ctx := context.Background()

	suDSN = os.Getenv("TEST_DATABASE_DSN")
	if suDSN == "" {
		fmt.Fprintln(os.Stderr, "TEST_DATABASE_DSN not set — run: task test:integration:db")
		os.Exit(1)
	}

	migrator, err := db.NewMigrator(suDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create migrator: %v\n", err)
		os.Exit(1)
	}
	if err := migrator.Up(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "failed to run migrations: %v\n", err)
		os.Exit(1)
	}
	if err := migrator.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to close migrator: %v\n", err)
		os.Exit(1)
	}

	adminDB, err := sql.Open("pgx", suDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to open admin connection: %v\n", err)
		os.Exit(1)
	}
	if _, err := adminDB.ExecContext(ctx, "ALTER ROLE app_user WITH PASSWORD 'apppass'"); err != nil {
		fmt.Fprintf(os.Stderr, "failed to set app_user password: %v\n", err)
		os.Exit(1)
	}
	if err := adminDB.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to close admin connection: %v\n", err)
		os.Exit(1)
	}

	suPool, err = db.NewPool(db.PoolConfig{
		DSN:          suDSN,
		MaxOpenConns: 5,
		Extensions:   []string{"age", "vector"},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create superuser pool: %v\n", err)
		os.Exit(1)
	}
	defer suPool.Close()

	// Verify migration 010 applied (controls table must exist).
	err = suPool.Exec(ctx, "SELECT 1 FROM controls LIMIT 0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "controls table does not exist — migration 010 may not have applied.\n"+
			"If the schema_migrations table is dirty, run: task test:integration:clean\n"+
			"Error: %v\n", err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

func appUserDSN() string {
	u, err := url.Parse(suDSN)
	if err != nil {
		panic(fmt.Sprintf("bad suDSN: %v", err))
	}
	u.User = url.UserPassword("app_user", "apppass")
	return u.String()
}

func appUserConn(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("pgx", appUserDSN())
	if err != nil {
		t.Fatalf("failed to open app_user connection: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// tenantConn returns a db.Connection backed by a TenantPool. Every
// transaction started via Begin() automatically sets app.current_tenant
// from the context, enforcing PostgreSQL RLS without session-level workarounds.
func tenantConn(t *testing.T) db.Connection {
	t.Helper()
	pool, err := db.NewPool(db.PoolConfig{
		DSN:          appUserDSN(),
		MaxOpenConns: 2,
	})
	if err != nil {
		t.Fatalf("failed to create app_user pool: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	return db.NewTenantPool(pool)
}

func setupTenant(t *testing.T, tenantID, displayName string) {
	t.Helper()
	ctx := context.Background()
	err := suPool.Exec(ctx,
		"INSERT INTO tenants (tenant_id, display_name) VALUES ($1, $2) ON CONFLICT DO NOTHING",
		tenantID, displayName)
	if err != nil {
		t.Fatalf("setupTenant(%q): %v", tenantID, err)
	}
}

func setupCatalog(t *testing.T, conn *sql.DB, tenantID, catalogID, name string) {
	t.Helper()
	ctx := context.Background()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, "SELECT set_config('app.current_tenant', $1, true)", tenantID); err != nil {
		t.Fatalf("set_config: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO catalogs (catalog_id, tenant_id, name, version, source_type, object_path) VALUES ($1, $2, $3, '1.0', 'oscal', 'test') ON CONFLICT DO NOTHING",
		catalogID, tenantID, name); err != nil {
		t.Fatalf("insert catalog: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func execAsTenant(t *testing.T, conn *sql.DB, tenantID string, fn func(tx *sql.Tx)) {
	t.Helper()
	ctx := context.Background()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, "SELECT set_config('app.current_tenant', $1, true)", tenantID); err != nil {
		t.Fatalf("set_config tenant: %v", err)
	}
	fn(tx)
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// --- Store integration tests ---

func TestIntegration_PGStore_UpsertAndGetCatalog(t *testing.T) {
	setupTenant(t, "cat-test-alpha", "Catalog Alpha")
	conn := tenantConn(t)
	rawConn := appUserConn(t)

	store := catalog.NewPGStore(conn)
	catalogID := fmt.Sprintf("cat-int-%d", time.Now().UnixNano())

	execAsTenant(t, rawConn, "cat-test-alpha", func(tx *sql.Tx) {
		// Use raw SQL to insert the catalog since Store methods don't set tenant context
		_, err := tx.ExecContext(context.Background(),
			`INSERT INTO catalogs (catalog_id, tenant_id, name, version, source_type, object_path,
				source_uri, content_hash, content_size, format, output_hash, extractor_name, extractor_version)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
			catalogID, "cat-test-alpha", "Test Catalog", "1.0", "oscal", "/test/path",
			"https://example.com/catalog.json", "abc123hash", 12345,
			"oscal-json", "def456hash", "oscal-parser", "0.1.0")
		if err != nil {
			t.Fatalf("insert catalog: %v", err)
		}
	})

	// Read it back (GetCatalog runs in the session context)
	execAsTenant(t, rawConn, "cat-test-alpha", func(tx *sql.Tx) {
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
		if err != nil {
			t.Fatalf("get catalog: %v", err)
		}
		if rec.CatalogID != catalogID {
			t.Errorf("CatalogID = %q, want %q", rec.CatalogID, catalogID)
		}
		if rec.Name != "Test Catalog" {
			t.Errorf("Name = %q, want %q", rec.Name, "Test Catalog")
		}
		if rec.ContentHash != "abc123hash" {
			t.Errorf("ContentHash = %q, want %q", rec.ContentHash, "abc123hash")
		}
		if rec.ContentSize != 12345 {
			t.Errorf("ContentSize = %d, want %d", rec.ContentSize, 12345)
		}
		if rec.ExtractorName != "oscal-parser" {
			t.Errorf("ExtractorName = %q, want %q", rec.ExtractorName, "oscal-parser")
		}
	})

	_ = store // compile check
}

func TestIntegration_PGStore_UpsertControls(t *testing.T) {
	tenantID := "ctrl-test-alpha"
	catalogID := fmt.Sprintf("ctrl-cat-%d", time.Now().UnixNano())

	setupTenant(t, tenantID, "Control Alpha")
	rawConn := appUserConn(t)

	// First create the catalog (controls have FK to catalogs)
	setupCatalog(t, rawConn, tenantID, catalogID, "Control Test Catalog")

	controls := []catalog.ControlRecord{
		{
			TenantID:   tenantID,
			ControlID:  fmt.Sprintf("%s/ac-1", catalogID),
			CatalogID:  catalogID,
			Identifier: "ac-1",
			Title:      "Policy and Procedures",
			Statement:  "Develop and maintain policy.",
			Class:      "compliance-requirement",
			GroupID:    "ac",
			Props:      map[string]string{"source": "nist"},
			CreatedAt:  time.Now().UTC(),
		},
		{
			TenantID:   tenantID,
			ControlID:  fmt.Sprintf("%s/ac-2", catalogID),
			CatalogID:  catalogID,
			Identifier: "ac-2",
			Title:      "Account Management",
			Statement:  "Manage information system accounts.",
			Class:      "compliance-section",
			GroupID:    "ac",
			Props:      map[string]string{},
			CreatedAt:  time.Now().UTC(),
		},
		{
			TenantID:   tenantID,
			ControlID:  fmt.Sprintf("%s/ac-2.a", catalogID),
			CatalogID:  catalogID,
			Identifier: "ac-2.a",
			Title:      "",
			Statement:  "Identify account types.",
			Class:      "compliance-requirement",
			ParentID:   "ac-2",
			GroupID:    "ac",
			Props:      map[string]string{"parent-id": "ac-2"},
			CreatedAt:  time.Now().UTC(),
		},
	}

	conn := tenantConn(t)
	store := catalog.NewPGStore(conn)
	ctx, err := tenant.WithTenant(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("WithTenant: %v", err)
	}

	if err := store.UpsertControls(ctx, controls); err != nil {
		t.Fatalf("UpsertControls: %v", err)
	}

	// Verify controls are persisted (raw SQL in transaction is fine for verification)
	execAsTenant(t, rawConn, tenantID, func(tx *sql.Tx) {
		var count int
		err := tx.QueryRowContext(context.Background(),
			"SELECT COUNT(*) FROM controls WHERE catalog_id = $1", catalogID).Scan(&count)
		if err != nil {
			t.Fatalf("count controls: %v", err)
		}
		if count != 3 {
			t.Errorf("control count = %d, want 3", count)
		}
	})

	// Verify props stored as JSONB
	execAsTenant(t, rawConn, tenantID, func(tx *sql.Tx) {
		var propsJSON []byte
		err := tx.QueryRowContext(context.Background(),
			"SELECT props FROM controls WHERE control_id = $1",
			fmt.Sprintf("%s/ac-1", catalogID)).Scan(&propsJSON)
		if err != nil {
			t.Fatalf("get props: %v", err)
		}
		var props map[string]string
		if err := json.Unmarshal(propsJSON, &props); err != nil {
			t.Fatalf("unmarshal props: %v", err)
		}
		if props["source"] != "nist" {
			t.Errorf("props[source] = %q, want %q", props["source"], "nist")
		}
	})
}

func TestIntegration_PGStore_UpsertControls_UpdateExisting(t *testing.T) {
	tenantID := "upsert-test"
	catalogID := fmt.Sprintf("upsert-cat-%d", time.Now().UnixNano())

	setupTenant(t, tenantID, "Upsert Test")
	rawConn := appUserConn(t)
	setupCatalog(t, rawConn, tenantID, catalogID, "Upsert Catalog")

	conn := tenantConn(t)
	store := catalog.NewPGStore(conn)
	ctx, err := tenant.WithTenant(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("WithTenant: %v", err)
	}
	controlID := fmt.Sprintf("%s/ac-1", catalogID)

	// Insert initial version
	initial := []catalog.ControlRecord{{
		TenantID:   tenantID,
		ControlID:  controlID,
		CatalogID:  catalogID,
		Identifier: "ac-1",
		Title:      "Original Title",
		Statement:  "Original statement.",
		Class:      "compliance-requirement",
		Props:      map[string]string{},
		CreatedAt:  time.Now().UTC(),
	}}

	if err := store.UpsertControls(ctx, initial); err != nil {
		t.Fatalf("initial insert: %v", err)
	}

	// Upsert with updated title and statement
	updated := []catalog.ControlRecord{{
		TenantID:   tenantID,
		ControlID:  controlID,
		CatalogID:  catalogID,
		Identifier: "ac-1",
		Title:      "Updated Title",
		Statement:  "Updated statement.",
		Class:      "compliance-requirement",
		Props:      map[string]string{"version": "2"},
		CreatedAt:  time.Now().UTC(),
	}}

	if err := store.UpsertControls(ctx, updated); err != nil {
		t.Fatalf("upsert update: %v", err)
	}

	// Verify the update took effect
	execAsTenant(t, rawConn, tenantID, func(tx *sql.Tx) {
		var title, statement string
		err := tx.QueryRowContext(context.Background(),
			"SELECT title, statement FROM controls WHERE control_id = $1", controlID).Scan(&title, &statement)
		if err != nil {
			t.Fatalf("read updated control: %v", err)
		}
		if title != "Updated Title" {
			t.Errorf("title = %q, want %q", title, "Updated Title")
		}
		if statement != "Updated statement." {
			t.Errorf("statement = %q, want %q", statement, "Updated statement.")
		}
	})

	// Verify there's still only 1 control (not 2)
	execAsTenant(t, rawConn, tenantID, func(tx *sql.Tx) {
		var count int
		err := tx.QueryRowContext(context.Background(),
			"SELECT COUNT(*) FROM controls WHERE control_id = $1", controlID).Scan(&count)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		if count != 1 {
			t.Errorf("count = %d, want 1 (upsert should update, not duplicate)", count)
		}
	})
}

func TestIntegration_PGStore_FullTextSearch(t *testing.T) {
	tenantID := "fts-test"
	catalogID := fmt.Sprintf("fts-cat-%d", time.Now().UnixNano())

	setupTenant(t, tenantID, "FTS Test")
	rawConn := appUserConn(t)
	setupCatalog(t, rawConn, tenantID, catalogID, "FTS Catalog")

	controls := []catalog.ControlRecord{
		{
			TenantID:   tenantID,
			ControlID:  fmt.Sprintf("%s/ac-1", catalogID),
			CatalogID:  catalogID,
			Identifier: "ac-1",
			Title:      "Access Control Policy",
			Statement:  "Develop, document, and disseminate an access control policy.",
			Class:      "compliance-requirement",
			Props:      map[string]string{},
			CreatedAt:  time.Now().UTC(),
		},
		{
			TenantID:   tenantID,
			ControlID:  fmt.Sprintf("%s/au-1", catalogID),
			CatalogID:  catalogID,
			Identifier: "au-1",
			Title:      "Audit Policy",
			Statement:  "Develop, document, and disseminate an audit and accountability policy.",
			Class:      "compliance-requirement",
			Props:      map[string]string{},
			CreatedAt:  time.Now().UTC(),
		},
		{
			TenantID:   tenantID,
			ControlID:  fmt.Sprintf("%s/si-1", catalogID),
			CatalogID:  catalogID,
			Identifier: "si-1",
			Title:      "System and Information Integrity Policy",
			Statement:  "Develop system integrity procedures for malicious code protection.",
			Class:      "compliance-requirement",
			Props:      map[string]string{},
			CreatedAt:  time.Now().UTC(),
		},
	}

	conn := tenantConn(t)
	store := catalog.NewPGStore(conn)
	ctx, err := tenant.WithTenant(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("WithTenant: %v", err)
	}
	if err := store.UpsertControls(ctx, controls); err != nil {
		t.Fatalf("UpsertControls: %v", err)
	}

	// Search for "access control" — should rank ac-1 highest
	execAsTenant(t, rawConn, tenantID, func(tx *sql.Tx) {
		rows, err := tx.QueryContext(context.Background(), `
			SELECT control_id, ts_rank_cd(search_vector, plainto_tsquery('english', $1)) AS rank
			FROM controls
			WHERE search_vector @@ plainto_tsquery('english', $1)
			ORDER BY rank DESC`, "access control")
		if err != nil {
			t.Fatalf("FTS query: %v", err)
		}
		defer rows.Close()

		var results []struct {
			ControlID string
			Rank      float64
		}
		for rows.Next() {
			var r struct {
				ControlID string
				Rank      float64
			}
			if err := rows.Scan(&r.ControlID, &r.Rank); err != nil {
				t.Fatalf("scan: %v", err)
			}
			results = append(results, r)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows err: %v", err)
		}

		if len(results) == 0 {
			t.Fatal("FTS returned 0 results for 'access control'")
		}

		// First result should be ac-1 (has "access control" in both title and statement)
		if !strings.HasSuffix(results[0].ControlID, "/ac-1") {
			t.Errorf("top result = %q, want ac-1", results[0].ControlID)
		}
	})

	// Search for "malicious code" — should match si-1 only
	execAsTenant(t, rawConn, tenantID, func(tx *sql.Tx) {
		var count int
		err := tx.QueryRowContext(context.Background(), `
			SELECT COUNT(*) FROM controls
			WHERE search_vector @@ plainto_tsquery('english', $1)`, "malicious code").Scan(&count)
		if err != nil {
			t.Fatalf("FTS count: %v", err)
		}
		if count != 1 {
			t.Errorf("FTS 'malicious code' matched %d controls, want 1", count)
		}
	})

	// Search for "nonexistent term" — should return 0 results
	execAsTenant(t, rawConn, tenantID, func(tx *sql.Tx) {
		var count int
		err := tx.QueryRowContext(context.Background(), `
			SELECT COUNT(*) FROM controls
			WHERE search_vector @@ plainto_tsquery('english', $1)`, "xyznonexistent").Scan(&count)
		if err != nil {
			t.Fatalf("FTS count: %v", err)
		}
		if count != 0 {
			t.Errorf("FTS 'xyznonexistent' matched %d controls, want 0", count)
		}
	})
}

func TestIntegration_PGStore_TenantIsolation(t *testing.T) {
	tenantAlpha := "cat-iso-alpha"
	tenantBeta := "cat-iso-beta"
	catalogAlpha := fmt.Sprintf("iso-cat-alpha-%d", time.Now().UnixNano())
	catalogBeta := fmt.Sprintf("iso-cat-beta-%d", time.Now().UnixNano())

	setupTenant(t, tenantAlpha, "Alpha Corp")
	setupTenant(t, tenantBeta, "Beta Inc")
	conn := appUserConn(t)
	setupCatalog(t, conn, tenantAlpha, catalogAlpha, "Alpha Catalog")
	setupCatalog(t, conn, tenantBeta, catalogBeta, "Beta Catalog")

	// Insert control for alpha
	execAsTenant(t, conn, tenantAlpha, func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(), `
			INSERT INTO controls (tenant_id, control_id, catalog_id, identifier, title, statement, class)
			VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT DO NOTHING`,
			tenantAlpha, catalogAlpha+"/ac-1", catalogAlpha, "ac-1",
			"Alpha Control", "Alpha policy.", "compliance-requirement")
		if err != nil {
			t.Fatalf("insert alpha control: %v", err)
		}
	})

	// Insert control for beta
	execAsTenant(t, conn, tenantBeta, func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(), `
			INSERT INTO controls (tenant_id, control_id, catalog_id, identifier, title, statement, class)
			VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT DO NOTHING`,
			tenantBeta, catalogBeta+"/si-1", catalogBeta, "si-1",
			"Beta Control", "Beta integrity.", "compliance-requirement")
		if err != nil {
			t.Fatalf("insert beta control: %v", err)
		}
	})

	// Query as alpha: should see only alpha's controls
	execAsTenant(t, conn, tenantAlpha, func(tx *sql.Tx) {
		rows, err := tx.QueryContext(context.Background(), "SELECT control_id, tenant_id FROM controls")
		if err != nil {
			t.Fatalf("query alpha controls: %v", err)
		}
		defer rows.Close()

		var ids []string
		for rows.Next() {
			var id, tid string
			if err := rows.Scan(&id, &tid); err != nil {
				t.Fatalf("scan: %v", err)
			}
			ids = append(ids, id)
			if tid != tenantAlpha {
				t.Errorf("alpha session sees non-alpha tenant_id: %s (control %s)", tid, id)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows err: %v", err)
		}

		for _, id := range ids {
			if strings.Contains(id, "beta") || strings.Contains(id, tenantBeta) {
				t.Errorf("alpha tenant sees beta control: %s", id)
			}
		}
	})

	// Query as beta: should see only beta's controls
	execAsTenant(t, conn, tenantBeta, func(tx *sql.Tx) {
		rows, err := tx.QueryContext(context.Background(), "SELECT control_id FROM controls")
		if err != nil {
			t.Fatalf("query beta controls: %v", err)
		}
		defer rows.Close()

		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if strings.Contains(id, "alpha") || strings.Contains(id, tenantAlpha) {
				t.Errorf("beta tenant sees alpha control: %s", id)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows err: %v", err)
		}
	})

	// No tenant context: should see zero controls
	var count int
	err := conn.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM controls").Scan(&count)
	if err != nil {
		t.Fatalf("no-context query: %v", err)
	}
	if count != 0 {
		t.Errorf("no-context query returned %d controls, want 0 (RLS fail-closed)", count)
	}
}

func TestIntegration_PGStore_SearchViaStore(t *testing.T) {
	tenantID := "store-search-test"
	catalogID := fmt.Sprintf("store-search-cat-%d", time.Now().UnixNano())

	setupTenant(t, tenantID, "Store Search")
	rawConn := appUserConn(t)
	setupCatalog(t, rawConn, tenantID, catalogID, "Search Catalog")

	conn := tenantConn(t)
	store := catalog.NewPGStore(conn)
	ctx, err := tenant.WithTenant(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("WithTenant: %v", err)
	}

	controls := []catalog.ControlRecord{
		{
			TenantID:   tenantID,
			ControlID:  fmt.Sprintf("%s/ac-1", catalogID),
			CatalogID:  catalogID,
			Identifier: "ac-1",
			Title:      "Access Control Policy",
			Statement:  "Develop and disseminate an access control policy.",
			Class:      "compliance-requirement",
			Props:      map[string]string{},
			CreatedAt:  time.Now().UTC(),
		},
		{
			TenantID:   tenantID,
			ControlID:  fmt.Sprintf("%s/au-1", catalogID),
			CatalogID:  catalogID,
			Identifier: "au-1",
			Title:      "Audit Policy",
			Statement:  "Maintain audit logs for all system events.",
			Class:      "compliance-requirement",
			Props:      map[string]string{},
			CreatedAt:  time.Now().UTC(),
		},
	}

	if err := store.UpsertControls(ctx, controls); err != nil {
		t.Fatalf("UpsertControls: %v", err)
	}

	// Search via Store interface (tenant set in context via TenantPool)
	results, pageInfo, err := store.SearchControls(ctx, catalog.SearchQuery{
		Query: "access control policy",
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("SearchControls: %v", err)
	}

	if len(results) == 0 {
		t.Fatal("SearchControls returned 0 results")
	}

	found := false
	for _, r := range results {
		if r.Identifier == "ac-1" {
			found = true
		}
	}
	if !found {
		t.Error("ac-1 not found in search results")
	}

	if pageInfo.TotalCount < 1 {
		t.Errorf("TotalCount = %d, want >= 1", pageInfo.TotalCount)
	}
}

func TestIntegration_PGStore_CatalogProvenanceColumns(t *testing.T) {
	tenantID := "prov-test"
	catalogID := fmt.Sprintf("prov-cat-%d", time.Now().UnixNano())

	setupTenant(t, tenantID, "Provenance Test")
	conn := appUserConn(t)

	// Insert catalog with provenance columns
	execAsTenant(t, conn, tenantID, func(tx *sql.Tx) {
		_, err := tx.ExecContext(context.Background(), `
			INSERT INTO catalogs (catalog_id, tenant_id, name, version, source_type, object_path,
				source_uri, content_hash, content_size, format, output_hash, extractor_name, extractor_version)
			VALUES ($1, $2, 'Provenance Catalog', '1.0', 'oscal', '/test',
				'https://nist.gov/sp800-53', 'sha256abcdef', 42000,
				'oscal-json', 'sha256output', 'oscal-parser', '0.2.0')`,
			catalogID, tenantID)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	})

	// Read back and verify provenance columns
	execAsTenant(t, conn, tenantID, func(tx *sql.Tx) {
		var sourceURI, contentHash, format, outputHash, extractorName, extractorVersion string
		var contentSize int64
		err := tx.QueryRowContext(context.Background(), `
			SELECT source_uri, content_hash, content_size, format, output_hash, extractor_name, extractor_version
			FROM catalogs WHERE catalog_id = $1`, catalogID).Scan(
			&sourceURI, &contentHash, &contentSize, &format, &outputHash, &extractorName, &extractorVersion)
		if err != nil {
			t.Fatalf("read provenance: %v", err)
		}
		if sourceURI != "https://nist.gov/sp800-53" {
			t.Errorf("source_uri = %q", sourceURI)
		}
		if contentHash != "sha256abcdef" {
			t.Errorf("content_hash = %q", contentHash)
		}
		if contentSize != 42000 {
			t.Errorf("content_size = %d", contentSize)
		}
		if format != "oscal-json" {
			t.Errorf("format = %q", format)
		}
		if extractorName != "oscal-parser" {
			t.Errorf("extractor_name = %q", extractorName)
		}
	})
}

// --- Store-abstraction tests ---
// These exercise the Store interface methods directly against PostgreSQL.

func TestIntegration_PGStore_UpsertCatalogViaStore(t *testing.T) {
	tenantID := "store-upsert-cat"
	setupTenant(t, tenantID, "Store Upsert Catalog")
	conn := tenantConn(t)
	store := catalog.NewPGStore(conn)
	ctx, err := tenant.WithTenant(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("WithTenant: %v", err)
	}
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

	if err := store.UpsertCatalog(ctx, rec); err != nil {
		t.Fatalf("UpsertCatalog: %v", err)
	}

	got, err := store.GetCatalog(ctx, catalogID)
	if err != nil {
		t.Fatalf("GetCatalog: %v", err)
	}
	if got.Name != "Store Test Catalog" {
		t.Errorf("Name = %q, want %q", got.Name, "Store Test Catalog")
	}
	if got.Version != "2.0" {
		t.Errorf("Version = %q, want %q", got.Version, "2.0")
	}
	if got.SourceURI != "https://example.com/store" {
		t.Errorf("SourceURI = %q", got.SourceURI)
	}
	if got.ContentSize != 99999 {
		t.Errorf("ContentSize = %d, want 99999", got.ContentSize)
	}
	if got.ExtractorName != "store-parser" {
		t.Errorf("ExtractorName = %q", got.ExtractorName)
	}

	// Upsert again with updated name — should update, not create new
	rec.Name = "Updated Catalog Name"
	if err := store.UpsertCatalog(ctx, rec); err != nil {
		t.Fatalf("UpsertCatalog (update): %v", err)
	}

	got, err = store.GetCatalog(ctx, catalogID)
	if err != nil {
		t.Fatalf("GetCatalog after update: %v", err)
	}
	if got.Name != "Updated Catalog Name" {
		t.Errorf("Name after update = %q, want %q", got.Name, "Updated Catalog Name")
	}
}

func TestIntegration_PGStore_GetCatalog_NotFound(t *testing.T) {
	tenantID := "store-notfound"
	setupTenant(t, tenantID, "Not Found Test")
	conn := tenantConn(t)
	store := catalog.NewPGStore(conn)
	ctx, err := tenant.WithTenant(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("WithTenant: %v", err)
	}

	_, err = store.GetCatalog(ctx, "nonexistent-catalog-id")
	if err == nil {
		t.Fatal("expected error for nonexistent catalog")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want to contain 'not found'", err.Error())
	}
}

func TestIntegration_PGStore_ListCatalogs(t *testing.T) {
	tenantID := "store-list-test"
	setupTenant(t, tenantID, "List Test")
	conn := tenantConn(t)
	store := catalog.NewPGStore(conn)
	ctx, err := tenant.WithTenant(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("WithTenant: %v", err)
	}

	// Insert 3 catalogs
	for i := 0; i < 3; i++ {
		catID := fmt.Sprintf("list-cat-%d-%d", time.Now().UnixNano(), i)
		func() {
			rec := catalog.CatalogRecord{
				CatalogID:  catID,
				TenantID:   tenantID,
				Name:       fmt.Sprintf("Catalog %d", i),
				Version:    "1.0",
				SourceType: "oscal",
				ObjectPath: "/list/test",
				CreatedAt:  time.Now().UTC(),
			}
			if err := store.UpsertCatalog(ctx, rec); err != nil {
				t.Fatalf("UpsertCatalog %d: %v", i, err)
			}
		}()
	}

	// List with default options
	records, pageInfo, err := store.ListCatalogs(ctx, catalog.ListOptions{})
	if err != nil {
		t.Fatalf("ListCatalogs: %v", err)
	}
	if len(records) < 3 {
		t.Errorf("ListCatalogs returned %d records, want >= 3", len(records))
	}
	if pageInfo.TotalCount < 3 {
		t.Errorf("TotalCount = %d, want >= 3", pageInfo.TotalCount)
	}

	// List with limit=1
	records, pageInfo, err = store.ListCatalogs(ctx, catalog.ListOptions{Limit: 1})
	if err != nil {
		t.Fatalf("ListCatalogs limit=1: %v", err)
	}
	if len(records) != 1 {
		t.Errorf("ListCatalogs limit=1 returned %d records, want 1", len(records))
	}
	if pageInfo.NextOffset != 1 {
		t.Errorf("NextOffset = %d, want 1", pageInfo.NextOffset)
	}
}

func TestIntegration_PGStore_GetControlViaStore(t *testing.T) {
	tenantID := "store-getctrl"
	catalogID := fmt.Sprintf("getctrl-cat-%d", time.Now().UnixNano())
	setupTenant(t, tenantID, "Get Control")
	rawConn := appUserConn(t)
	setupCatalog(t, rawConn, tenantID, catalogID, "GetCtrl Catalog")
	conn := tenantConn(t)
	store := catalog.NewPGStore(conn)
	ctx, err := tenant.WithTenant(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("WithTenant: %v", err)
	}

	controlID := fmt.Sprintf("%s/ac-1", catalogID)

	if err := store.UpsertControls(ctx, []catalog.ControlRecord{{
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
	}}); err != nil {
		t.Fatalf("UpsertControls: %v", err)
	}

	got, err := store.GetControl(ctx, controlID)
	if err != nil {
		t.Fatalf("GetControl: %v", err)
	}
	if got.Identifier != "ac-1" {
		t.Errorf("Identifier = %q", got.Identifier)
	}
	if got.Title != "Access Control Policy" {
		t.Errorf("Title = %q", got.Title)
	}
	if got.Class != "compliance-requirement" {
		t.Errorf("Class = %q", got.Class)
	}
	if got.GroupID != "ac" {
		t.Errorf("GroupID = %q", got.GroupID)
	}
	if got.Props["framework"] != "nist" {
		t.Errorf("Props[framework] = %q", got.Props["framework"])
	}

	// GetControl for nonexistent
	_, err = store.GetControl(ctx, "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent control")
	}
}

func TestIntegration_PGStore_FTSWeightedRanking(t *testing.T) {
	tenantID := "fts-weight-test"
	catalogID := fmt.Sprintf("fts-weight-%d", time.Now().UnixNano())

	setupTenant(t, tenantID, "FTS Weight")
	rawConn := appUserConn(t)
	setupCatalog(t, rawConn, tenantID, catalogID, "Weight Catalog")

	controls := []catalog.ControlRecord{
		{
			TenantID:   tenantID,
			ControlID:  fmt.Sprintf("%s/title-match", catalogID),
			CatalogID:  catalogID,
			Identifier: "title-match",
			Title:      "Encryption Policy",
			Statement:  "Implement data protection measures.",
			Class:      "compliance-requirement",
			Props:      map[string]string{},
			CreatedAt:  time.Now().UTC(),
		},
		{
			TenantID:   tenantID,
			ControlID:  fmt.Sprintf("%s/body-match", catalogID),
			CatalogID:  catalogID,
			Identifier: "body-match",
			Title:      "Data Protection",
			Statement:  "Apply encryption to all data at rest and in transit.",
			Class:      "compliance-requirement",
			Props:      map[string]string{},
			CreatedAt:  time.Now().UTC(),
		},
	}

	conn := tenantConn(t)
	store := catalog.NewPGStore(conn)
	ctx, err := tenant.WithTenant(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("WithTenant: %v", err)
	}
	if err := store.UpsertControls(ctx, controls); err != nil {
		t.Fatalf("UpsertControls: %v", err)
	}

	// Search for "encryption" — title match (weight A) should rank higher than body match (weight B)
	execAsTenant(t, rawConn, tenantID, func(tx *sql.Tx) {
		rows, err := tx.QueryContext(context.Background(), `
			SELECT identifier, ts_rank_cd(search_vector, plainto_tsquery('english', $1)) AS rank
			FROM controls
			WHERE catalog_id = $2 AND search_vector @@ plainto_tsquery('english', $1)
			ORDER BY rank DESC`, "encryption", catalogID)
		if err != nil {
			t.Fatalf("FTS: %v", err)
		}
		defer rows.Close()

		var results []struct {
			Identifier string
			Rank       float64
		}
		for rows.Next() {
			var r struct {
				Identifier string
				Rank       float64
			}
			if err := rows.Scan(&r.Identifier, &r.Rank); err != nil {
				t.Fatalf("scan: %v", err)
			}
			results = append(results, r)
		}

		if len(results) < 2 {
			t.Fatalf("expected 2 results, got %d", len(results))
		}

		// Title match should rank higher
		if results[0].Identifier != "title-match" {
			t.Errorf("expected title-match to rank first, got %q (rank %.4f) before %q (rank %.4f)",
				results[0].Identifier, results[0].Rank,
				results[1].Identifier, results[1].Rank)
		}
	})
}

// --- End-to-end: OSCAL JSON → Parser → Store → Search ---

func TestIntegration_E2E_ParseCatalogAndSearch(t *testing.T) {
	tenantID := "e2e-oscal"
	setupTenant(t, tenantID, "E2E OSCAL")
	rawConn := appUserConn(t)

	// Parse the test fixture through the real Parser.
	fixtureData, err := os.ReadFile("../../pkg/oscal/testdata/minimal_catalog.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	parser := oscal.NewParser("")
	items, err := parser.Parse(context.Background(), bytes.NewReader(fixtureData))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(items) == 0 {
		t.Fatal("parser returned 0 items")
	}
	t.Logf("parser produced %d ControlItems", len(items))

	// Verify parser produced expected controls.
	ids := make(map[string]bool)
	for _, item := range items {
		ids[item.ID] = true
	}
	for _, want := range []string{"ac-1", "ac-2", "ac-3", "ac-2.1"} {
		if !ids[want] {
			t.Errorf("expected control %q in parsed output, not found", want)
		}
	}

	// Verify AC-2 was decomposed (parent + children).
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
	if ac2Parent == nil {
		t.Fatal("ac-2 not found in parsed output")
	}
	if ac2Parent.Class != oscal.ClassSection {
		t.Errorf("ac-2.Class = %q, want %q (decomposed parent should be section)", ac2Parent.Class, oscal.ClassSection)
	}
	if ac2Children == 0 {
		t.Error("ac-2 has no children — decomposition did not produce child items")
	}
	t.Logf("ac-2 decomposed: parent=%s, children=%d", ac2Parent.Class, ac2Children)

	// Verify parameter substitution in AC-1.
	ac1, err := parser.FindControl(items, "ac-1")
	if err != nil {
		t.Fatalf("FindControl ac-1: %v", err)
	}
	if strings.Contains(ac1.Text, "{{ insert:") {
		t.Errorf("ac-1 still has unresolved template: %q", ac1.Text)
	}
	if !strings.Contains(ac1.Text, "organization-defined personnel or roles") {
		t.Errorf("ac-1 text missing substituted param label: %q", ac1.Text)
	}
	t.Logf("ac-1 text: %s", ac1.Text)

	// Persist: create catalog, then upsert controls via Store.
	catalogID := fmt.Sprintf("e2e-cat-%d", time.Now().UnixNano())
	conn := tenantConn(t)
	store := catalog.NewPGStore(conn)
	ctx, err := tenant.WithTenant(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("WithTenant: %v", err)
	}

	func() {
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
		if err := store.UpsertCatalog(ctx, catRec); err != nil {
			t.Fatalf("UpsertCatalog: %v", err)
		}
	}()

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

	if err := store.UpsertControls(ctx, controlRecords); err != nil {
		t.Fatalf("UpsertControls: %v", err)
	}

	// Verify all controls persisted.
	execAsTenant(t, rawConn, tenantID, func(tx *sql.Tx) {
		var count int
		err := tx.QueryRowContext(context.Background(),
			"SELECT COUNT(*) FROM controls WHERE catalog_id = $1", catalogID).Scan(&count)
		if err != nil {
			t.Fatalf("count controls: %v", err)
		}
		if count != len(items) {
			t.Errorf("persisted %d controls, want %d", count, len(items))
		}
		t.Logf("persisted %d controls to catalog %s", count, catalogID)
	})

	// Full-text search: "account management" should find AC-2 controls.
	{
		results, _, err := store.SearchControls(ctx, catalog.SearchQuery{
			Query:      "account management",
			CatalogIDs: []string{catalogID},
			Limit:      20,
		})
		if err != nil {
			t.Fatalf("SearchControls 'account management': %v", err)
		}
		if len(results) == 0 {
			t.Fatal("search 'account management' returned 0 results")
		}

		foundAC2 := false
		for _, r := range results {
			if r.Identifier == "ac-2" || strings.HasPrefix(r.Identifier, "ac-2.") {
				foundAC2 = true
			}
		}
		if !foundAC2 {
			resultIDs := make([]string, len(results))
			for i, r := range results {
				resultIDs[i] = r.Identifier
			}
			t.Errorf("search 'account management' did not find ac-2; got: %v", resultIDs)
		}
		t.Logf("search 'account management' returned %d results", len(results))
	}

	// Full-text search: "access enforcement" should find AC-3.
	{
		results, _, err := store.SearchControls(ctx, catalog.SearchQuery{
			Query:      "access enforcement",
			CatalogIDs: []string{catalogID},
			Limit:      20,
		})
		if err != nil {
			t.Fatalf("SearchControls 'access enforcement': %v", err)
		}

		foundAC3 := false
		for _, r := range results {
			if r.Identifier == "ac-3" {
				foundAC3 = true
			}
		}
		if !foundAC3 {
			t.Errorf("search 'access enforcement' did not find ac-3")
		}
	}

	// Full-text search: "automated mechanism" should find AC-2.1 (sub-control).
	{
		results, _, err := store.SearchControls(ctx, catalog.SearchQuery{
			Query:      "automated mechanisms",
			CatalogIDs: []string{catalogID},
			Limit:      20,
		})
		if err != nil {
			t.Fatalf("SearchControls 'automated mechanisms': %v", err)
		}

		foundAC21 := false
		for _, r := range results {
			if r.Identifier == "ac-2.1" {
				foundAC21 = true
			}
		}
		if !foundAC21 {
			t.Errorf("search 'automated mechanisms' did not find ac-2.1 (sub-control)")
		}
	}

	// Negative: unrelated term returns no results for this catalog.
	{
		results, _, err := store.SearchControls(ctx, catalog.SearchQuery{
			Query:      "blockchain cryptocurrency",
			CatalogIDs: []string{catalogID},
			Limit:      20,
		})
		if err != nil {
			t.Fatalf("SearchControls 'blockchain': %v", err)
		}
		if len(results) != 0 {
			t.Errorf("search 'blockchain cryptocurrency' returned %d results, want 0", len(results))
		}
	}

	// Re-import: upsert the same controls with updated titles.
	for i := range controlRecords {
		controlRecords[i].Title = controlRecords[i].Title + " (updated)"
	}
	if err := store.UpsertControls(ctx, controlRecords); err != nil {
		t.Fatalf("UpsertControls (re-import): %v", err)
	}

	// Verify count didn't change (upsert, not duplicate).
	execAsTenant(t, rawConn, tenantID, func(tx *sql.Tx) {
		var count int
		err := tx.QueryRowContext(context.Background(),
			"SELECT COUNT(*) FROM controls WHERE catalog_id = $1", catalogID).Scan(&count)
		if err != nil {
			t.Fatalf("count after re-import: %v", err)
		}
		if count != len(items) {
			t.Errorf("after re-import: %d controls, want %d (upsert should not duplicate)", count, len(items))
		}
	})

	// Verify title was updated.
	{
		got, err := store.GetControl(ctx, fmt.Sprintf("%s/ac-3", catalogID))
		if err != nil {
			t.Fatalf("GetControl ac-3: %v", err)
		}
		if !strings.HasSuffix(got.Title, "(updated)") {
			t.Errorf("ac-3 title = %q, want to end with '(updated)'", got.Title)
		}
	}

	// Tenant isolation: different tenant cannot see these controls.
	otherTenant := "e2e-other"
	setupTenant(t, otherTenant, "Other Tenant")
	execAsTenant(t, rawConn, otherTenant, func(tx *sql.Tx) {
		var count int
		err := tx.QueryRowContext(context.Background(),
			"SELECT COUNT(*) FROM controls WHERE catalog_id = $1", catalogID).Scan(&count)
		if err != nil {
			t.Fatalf("other tenant count: %v", err)
		}
		if count != 0 {
			t.Errorf("other tenant sees %d controls from e2e catalog, want 0", count)
		}
	})
}

func TestIntegration_E2E_ParserRejectsInvalidOSCAL(t *testing.T) {
	parser := oscal.NewParser("")

	t.Run("malformed JSON", func(t *testing.T) {
		_, err := parser.Parse(context.Background(), bytes.NewReader([]byte(`{not json`)))
		if err == nil {
			t.Fatal("expected error for malformed JSON")
		}
		if !strings.Contains(err.Error(), "parse") && !strings.Contains(err.Error(), "unmarshal") {
			t.Errorf("error = %q, expected parse/unmarshal error", err)
		}
	})

	t.Run("valid JSON but not a catalog", func(t *testing.T) {
		_, err := parser.Parse(context.Background(), bytes.NewReader([]byte(`{"profile":{"uuid":"x"}}`)))
		if err == nil {
			t.Fatal("expected error for non-catalog document")
		}
		if !strings.Contains(err.Error(), "format") {
			t.Errorf("error = %q, expected format-related error", err)
		}
	})

	t.Run("catalog with no groups or controls", func(t *testing.T) {
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
		if err == nil {
			t.Fatal("expected error for catalog with no controls")
		}
		if !strings.Contains(err.Error(), "no controls") && !strings.Contains(err.Error(), "NoControls") {
			t.Errorf("error = %q, expected no-controls error", err)
		}
	})

	t.Run("empty input", func(t *testing.T) {
		_, err := parser.Parse(context.Background(), bytes.NewReader([]byte{}))
		if err == nil {
			t.Fatal("expected error for empty input")
		}
	})

	t.Run("valid JSON object with null catalog", func(t *testing.T) {
		_, err := parser.Parse(context.Background(), bytes.NewReader([]byte(`{"catalog": null}`)))
		if err == nil {
			t.Fatal("expected error for null catalog")
		}
	})

	t.Run("array instead of object", func(t *testing.T) {
		_, err := parser.Parse(context.Background(), bytes.NewReader([]byte(`[1,2,3]`)))
		if err == nil {
			t.Fatal("expected error for array input")
		}
	})
}

func TestIntegration_E2E_UpsertSubstantiveChanges(t *testing.T) {
	tenantID := "e2e-upsert"
	setupTenant(t, tenantID, "E2E Upsert")
	rawConn := appUserConn(t)
	catalogID := fmt.Sprintf("e2e-upsert-cat-%d", time.Now().UnixNano())
	setupCatalog(t, rawConn, tenantID, catalogID, "Upsert E2E Catalog")
	conn := tenantConn(t)
	store := catalog.NewPGStore(conn)
	ctx, err := tenant.WithTenant(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("WithTenant: %v", err)
	}

	// Initial import: 3 controls, AC-2 is a section with one child.
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

	if err := store.UpsertControls(ctx, initial); err != nil {
		t.Fatalf("initial import: %v", err)
	}

	// Snapshot initial state for comparison.
	var initialAC1Stmt, initialAC2Class string
	var initialCreatedAt time.Time
	execAsTenant(t, rawConn, tenantID, func(tx *sql.Tx) {
		err := tx.QueryRowContext(context.Background(),
			"SELECT statement, created_at FROM controls WHERE control_id = $1",
			catalogID+"/ac-1").Scan(&initialAC1Stmt, &initialCreatedAt)
		if err != nil {
			t.Fatalf("snapshot ac-1: %v", err)
		}
		err = tx.QueryRowContext(context.Background(),
			"SELECT class FROM controls WHERE control_id = $1",
			catalogID+"/ac-2").Scan(&initialAC2Class)
		if err != nil {
			t.Fatalf("snapshot ac-2: %v", err)
		}
	})

	if initialAC1Stmt != "Develop access control policy." {
		t.Fatalf("initial ac-1 statement = %q", initialAC1Stmt)
	}
	if initialAC2Class != "compliance-section" {
		t.Fatalf("initial ac-2 class = %q", initialAC2Class)
	}

	// Re-import with substantive changes:
	// 1. AC-1 statement text changed
	// 2. AC-2 class changed from section to requirement (re-classified)
	// 3. AC-2.a statement changed
	// 4. AC-2.b is NEW (added in re-import)
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

	if err := store.UpsertControls(ctx, reimport); err != nil {
		t.Fatalf("re-import: %v", err)
	}

	// Verify: AC-1 statement text changed.
	execAsTenant(t, rawConn, tenantID, func(tx *sql.Tx) {
		var stmt string
		err := tx.QueryRowContext(context.Background(),
			"SELECT statement FROM controls WHERE control_id = $1",
			catalogID+"/ac-1").Scan(&stmt)
		if err != nil {
			t.Fatalf("read ac-1: %v", err)
		}
		if !strings.Contains(stmt, "purpose, scope, and roles") {
			t.Errorf("ac-1 statement not updated: %q", stmt)
		}
	})

	// Verify: AC-2 class changed from section to requirement.
	execAsTenant(t, rawConn, tenantID, func(tx *sql.Tx) {
		var class string
		err := tx.QueryRowContext(context.Background(),
			"SELECT class FROM controls WHERE control_id = $1",
			catalogID+"/ac-2").Scan(&class)
		if err != nil {
			t.Fatalf("read ac-2: %v", err)
		}
		if class != "compliance-requirement" {
			t.Errorf("ac-2 class not updated: got %q, want compliance-requirement", class)
		}
	})

	// Verify: AC-2.b was added (new control in re-import).
	execAsTenant(t, rawConn, tenantID, func(tx *sql.Tx) {
		var stmt string
		err := tx.QueryRowContext(context.Background(),
			"SELECT statement FROM controls WHERE control_id = $1",
			catalogID+"/ac-2.b").Scan(&stmt)
		if err != nil {
			t.Fatalf("read ac-2.b: %v", err)
		}
		if !strings.Contains(stmt, "account managers") {
			t.Errorf("ac-2.b statement = %q", stmt)
		}
	})

	// Verify: total count is now 4 (3 original + 1 new, not 7 duplicates).
	execAsTenant(t, rawConn, tenantID, func(tx *sql.Tx) {
		var count int
		err := tx.QueryRowContext(context.Background(),
			"SELECT COUNT(*) FROM controls WHERE catalog_id = $1", catalogID).Scan(&count)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		if count != 4 {
			t.Errorf("count = %d, want 4 (3 upserted + 1 new)", count)
		}
	})

	// Verify: created_at was NOT changed on upserted controls.
	execAsTenant(t, rawConn, tenantID, func(tx *sql.Tx) {
		var createdAt time.Time
		err := tx.QueryRowContext(context.Background(),
			"SELECT created_at FROM controls WHERE control_id = $1",
			catalogID+"/ac-1").Scan(&createdAt)
		if err != nil {
			t.Fatalf("read ac-1 created_at: %v", err)
		}
		if !createdAt.Equal(initialCreatedAt) {
			t.Errorf("ac-1 created_at changed from %v to %v (upsert should preserve)", initialCreatedAt, createdAt)
		}
	})

	// Verify: FTS indexes updated (new text is searchable).
	execAsTenant(t, rawConn, tenantID, func(tx *sql.Tx) {
		var count int
		err := tx.QueryRowContext(context.Background(), `
			SELECT COUNT(*) FROM controls
			WHERE catalog_id = $1 AND search_vector @@ plainto_tsquery('english', $2)`,
			catalogID, "purpose scope roles").Scan(&count)
		if err != nil {
			t.Fatalf("FTS after upsert: %v", err)
		}
		if count == 0 {
			t.Error("FTS 'purpose scope roles' returned 0 results — tsvector not updated after upsert")
		}
	})

	// Verify: new control AC-2.b is searchable.
	{
		results, _, err := store.SearchControls(ctx, catalog.SearchQuery{
			Query:      "account managers",
			CatalogIDs: []string{catalogID},
			Limit:      10,
		})
		if err != nil {
			t.Fatalf("SearchControls 'account managers': %v", err)
		}
		found := false
		for _, r := range results {
			if r.Identifier == "ac-2.b" {
				found = true
			}
		}
		if !found {
			t.Error("newly added ac-2.b not found via FTS 'account managers'")
		}
	}
}

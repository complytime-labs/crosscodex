//go:build integration

package db_test

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"sort"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
)

// tenant_admin (migration 007) provisions and suspends tenants only through
// SECURITY DEFINER functions; it holds no table privilege. One Ordered
// container sets the role's password once, so parallel nodes never race on
// ALTER ROLE. The suite entry point lives in db_integration_bdd_test.go.
var _ = Describe("tenant_admin role and functions", Ordered, func() {
	var (
		adminConn      *sql.DB
		appConn        *sql.DB
		tenantAdminDSN string
	)
	ctx := context.Background()

	// tenantRow reads a tenants row as the superuser (RLS hides it from app_user).
	tenantRow := func(id string) (name, status string, found bool) {
		err := suPool.QueryRow(ctx, "SELECT display_name, status FROM tenants WHERE tenant_id = $1", id).Scan(&name, &status)
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", false
		}
		Expect(err).NotTo(HaveOccurred())
		return name, status, true
	}
	graphExists := func(id string) bool {
		var n int
		Expect(suPool.QueryRow(ctx, "SELECT count(*) FROM ag_catalog.ag_graph WHERE name::text = $1", "crosscodex_"+id).Scan(&n)).To(Succeed())
		return n == 1
	}
	expectSQLState := func(err error, code string) {
		Expect(err).To(HaveOccurred())
		Expect(db.IsPgErrorCode(err, code)).To(BeTrue(), "want SQLSTATE %s, got %v", code, err)
	}

	BeforeAll(func() {
		if suPool == nil {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}
		pw, err := dbtest.NewRolePassword()
		Expect(err).NotTo(HaveOccurred())
		stmt, err := dbtest.AlterRolePasswordSQL("tenant_admin", pw)
		Expect(err).NotTo(HaveOccurred())
		Expect(suPool.Exec(ctx, stmt)).To(Succeed(), "set tenant_admin password")

		u, err := url.Parse(suDSN)
		Expect(err).NotTo(HaveOccurred())
		u.User = url.UserPassword("tenant_admin", pw)
		tenantAdminDSN = u.String()
		adminConn, err = sql.Open("pgx", tenantAdminDSN)
		Expect(err).NotTo(HaveOccurred())
		appConn = appUserConn()

		setupTenant("tadm-existing", "Existing")
	})

	AfterAll(func() {
		if adminConn != nil {
			adminConn.Close()
		}
		if suPool != nil {
			// The tenant_graph_drop trigger drops each tenant's graph.
			_ = suPool.Exec(ctx, "DELETE FROM tenants WHERE tenant_id LIKE 'tadm-%'")
		}
	})

	It("is a login role without SUPERUSER, BYPASSRLS or CREATEROLE", func() {
		var login, super, bypass, createRole bool
		Expect(suPool.QueryRow(ctx,
			"SELECT rolcanlogin, rolsuper, rolbypassrls, rolcreaterole FROM pg_roles WHERE rolname = 'tenant_admin'").
			Scan(&login, &super, &bypass, &createRole)).To(Succeed())
		Expect([]bool{login, super, bypass, createRole}).To(Equal([]bool{true, false, false, false}))
	})

	DescribeTable("tenant_admin is denied direct access",
		func(query string) {
			_, err := adminConn.ExecContext(ctx, query)
			expectSQLState(err, "42501")
		},
		Entry("reading tenants", "SELECT tenant_id FROM tenants"),
		Entry("inserting tenants", "INSERT INTO tenants (tenant_id, display_name) VALUES ('tadm-evil', 'x')"),
		Entry("bulk suspending tenants", "UPDATE tenants SET status = 'suspended'"),
		Entry("deleting tenants", "DELETE FROM tenants WHERE tenant_id = 'tadm-existing'"),
		Entry("truncating tenants", "TRUNCATE tenants"),
		Entry("disabling RLS", "ALTER TABLE tenants DISABLE ROW LEVEL SECURITY"),
		Entry("reading a child table", "SELECT count(*) FROM jobs"),
		Entry("creating a function in public", "CREATE FUNCTION public.tadm_shadow() RETURNS int LANGUAGE sql AS 'SELECT 1'"),
		Entry("calling the app_user-only status check", "SELECT tenant_is_active('tadm-existing')"),
	)

	DescribeTable("app_user cannot administer or enumerate tenants",
		func(query string) {
			_, err := appConn.ExecContext(ctx, query)
			expectSQLState(err, "42501")
		},
		Entry("provision_tenant", "SELECT provision_tenant('tadm-evil', 'x')"),
		Entry("set_tenant_status", "SELECT set_tenant_status('tadm-existing', 'suspended')"),
		Entry("list_tenants", "SELECT * FROM list_tenants()"),
	)

	It("leaves tenants unchanged after the denied statements", func() {
		name, status, found := tenantRow("tadm-existing")
		Expect(found).To(BeTrue())
		Expect([]string{name, status}).To(Equal([]string{"Existing", "active"}))
		_, _, found = tenantRow("tadm-evil")
		Expect(found).To(BeFalse())
	})

	DescribeTable("does not grant EXECUTE to PUBLIC",
		func(signature string) {
			var ok bool
			Expect(suPool.QueryRow(ctx, "SELECT has_function_privilege('public', $1, 'EXECUTE')", signature).Scan(&ok)).To(Succeed())
			Expect(ok).To(BeFalse(), "PUBLIC must not hold EXECUTE on %s", signature)
		},
		Entry(nil, "public.provision_tenant(text, text)"),
		Entry(nil, "public.set_tenant_status(text, text)"),
		Entry(nil, "public.list_tenants()"),
		Entry(nil, "public.tenant_is_active(text)"),
	)

	It("declares every function SECURITY DEFINER with a pinned search_path", func() {
		rows, err := suPool.Query(ctx, `
			SELECT proname, prosecdef, coalesce(array_to_string(proconfig, ';'), '')
			FROM pg_proc
			WHERE pronamespace = 'public'::regnamespace
			  AND proname IN ('provision_tenant', 'set_tenant_status', 'list_tenants', 'tenant_is_active')
			ORDER BY proname`)
		Expect(err).NotTo(HaveOccurred())
		defer rows.Close()
		var names []string
		for rows.Next() {
			var name, config string
			var definer bool
			Expect(rows.Scan(&name, &definer, &config)).To(Succeed())
			Expect(definer).To(BeTrue(), name)
			Expect(config).To(Equal("search_path=pg_catalog, public, pg_temp"), name)
			names = append(names, name)
		}
		Expect(rows.Err()).NotTo(HaveOccurred())
		Expect(names).To(Equal([]string{"list_tenants", "provision_tenant", "set_tenant_status", "tenant_is_active"}))
	})

	It("provisions a tenant and its graph, and re-provisioning renames without resuming", func() {
		var created bool
		Expect(adminConn.QueryRowContext(ctx, "SELECT provision_tenant('tadm-new', 'New Co')").Scan(&created)).To(Succeed())
		Expect(created).To(BeTrue())
		name, status, found := tenantRow("tadm-new")
		Expect(found).To(BeTrue())
		Expect([]string{name, status}).To(Equal([]string{"New Co", "active"}))
		Expect(graphExists("tadm-new")).To(BeTrue())

		var previous string
		Expect(adminConn.QueryRowContext(ctx, "SELECT set_tenant_status('tadm-new', 'suspended')").Scan(&previous)).To(Succeed())
		Expect(previous).To(Equal("active"))

		Expect(adminConn.QueryRowContext(ctx, "SELECT provision_tenant('tadm-new', 'New Co Renamed')").Scan(&created)).To(Succeed())
		Expect(created).To(BeFalse())
		name, status, _ = tenantRow("tadm-new")
		Expect([]string{name, status}).To(Equal([]string{"New Co Renamed", "suspended"}))
	})

	It("accepts a 52-character ID, the longest pkg/tenant allows", func() {
		id := "tadm-" + strings.Repeat("a", 47)
		var created bool
		Expect(adminConn.QueryRowContext(ctx, "SELECT provision_tenant($1, 'Long')", id).Scan(&created)).To(Succeed())
		Expect(created).To(BeTrue())
		Expect(graphExists(id)).To(BeTrue())
	})

	DescribeTable("provision_tenant rejects bad input with an actionable message and changes nothing",
		func(id, name, want string) {
			_, err := adminConn.ExecContext(ctx, "SELECT provision_tenant($1, $2)", id, name)
			expectSQLState(err, "22023")
			Expect(err.Error()).To(ContainSubstring(want))
			_, _, found := tenantRow(id)
			Expect(found).To(BeFalse())
			Expect(graphExists(id)).To(BeFalse())
		},
		Entry("uppercase and underscore", "Tadm_Bad", "x", "Choose an ID that matches"),
		Entry("53 characters", "tadm-"+strings.Repeat("a", 48), "x", "3-52 characters"),
		Entry("leading digit", "1tadm", "x", "starting with a letter"),
		Entry("trailing hyphen", "tadm-bad-", "x", "not ending with a hyphen"),
		Entry("empty display name", "tadm-blank", "   ", "display name is empty"),
		Entry("control character in display name", "tadm-ctrl", "a\tb", "control character"),
		Entry("C1 control character in display name", "tadm-c1ctrl", "a\u009bb", "control character"),
	)

	It("set_tenant_status rejects an unknown tenant and changes nothing", func() {
		_, err := adminConn.ExecContext(ctx, "SELECT set_tenant_status('tadm-nobody', 'suspended')")
		expectSQLState(err, "P0002")
		Expect(err.Error()).To(ContainSubstring(`does not exist`))
		Expect(err.Error()).To(ContainSubstring("crosscodexd admin tenant create"))
		_, _, found := tenantRow("tadm-nobody")
		Expect(found).To(BeFalse())
	})

	It("set_tenant_status rejects an unknown status and changes nothing", func() {
		_, err := adminConn.ExecContext(ctx, "SELECT set_tenant_status('tadm-existing', 'deleted')")
		expectSQLState(err, "22023")
		Expect(err.Error()).To(ContainSubstring(`must be 'active' or 'suspended'`))
		_, status, _ := tenantRow("tadm-existing")
		Expect(status).To(Equal("active"))
	})

	It("set_tenant_status to the current status is a no-op that returns it", func() {
		var previous string
		Expect(adminConn.QueryRowContext(ctx, "SELECT set_tenant_status('tadm-existing', 'active')").Scan(&previous)).To(Succeed())
		Expect(previous).To(Equal("active"))
		_, status, _ := tenantRow("tadm-existing")
		Expect(status).To(Equal("active"))
	})

	It("list_tenants returns every tenant ordered by ID", func() {
		rows, err := adminConn.QueryContext(ctx, "SELECT tenant_id, status FROM list_tenants() WHERE tenant_id LIKE 'tadm-%'")
		Expect(err).NotTo(HaveOccurred())
		defer rows.Close()
		var ids []string
		for rows.Next() {
			var id, status string
			Expect(rows.Scan(&id, &status)).To(Succeed())
			ids = append(ids, id)
		}
		Expect(rows.Err()).NotTo(HaveOccurred())
		Expect(ids).To(ContainElements("tadm-existing", "tadm-new"))
		Expect(sort.StringsAreSorted(ids)).To(BeTrue(), "ids: %v", ids)
	})

	It("ignores a temporary table that shadows tenants (search_path hijack)", func() {
		conn, err := adminConn.Conn(ctx)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(conn.Close) // runs after the DROP below (LIFO)
		// Schema-qualified table references in the functions are what defeat
		// the shadow table; the proconfig spec above proves the pinned search_path.
		_, err = conn.ExecContext(ctx, "CREATE TEMP TABLE tenants (tenant_id text, display_name text, status text, created_at timestamptz)")
		Expect(err).NotTo(HaveOccurred(), "tenant_admin needs PUBLIC's default TEMP privilege for this spec")
		DeferCleanup(func() {
			_, err := conn.ExecContext(ctx, "DROP TABLE pg_temp.tenants")
			Expect(err).NotTo(HaveOccurred())
		})

		var created bool
		Expect(conn.QueryRowContext(ctx, "SELECT provision_tenant('tadm-hijack', 'Hijack')").Scan(&created)).To(Succeed())
		Expect(created).To(BeTrue())

		var shadowRows int
		Expect(conn.QueryRowContext(ctx, "SELECT count(*) FROM pg_temp.tenants").Scan(&shadowRows)).To(Succeed())
		Expect(shadowRows).To(BeZero())
		_, _, found := tenantRow("tadm-hijack")
		Expect(found).To(BeTrue())
	})

	It("tenant_is_active is true only for an active tenant, as app_user", func() {
		appPool, err := db.NewPool(db.PoolConfig{DSN: appUserDSN()})
		Expect(err).NotTo(HaveOccurred())
		defer appPool.Close()

		active, err := db.TenantActive(ctx, appPool, "tadm-existing")
		Expect(err).NotTo(HaveOccurred())
		Expect(active).To(BeTrue())

		active, err = db.TenantActive(ctx, appPool, "tadm-new") // suspended above
		Expect(err).NotTo(HaveOccurred())
		Expect(active).To(BeFalse())

		active, err = db.TenantActive(ctx, appPool, "tadm-nobody")
		Expect(err).NotTo(HaveOccurred())
		Expect(active).To(BeFalse())
	})

	Context("through TenantAdmin as tenant_admin", Ordered, func() {
		var admin *db.TenantAdmin

		BeforeAll(func() {
			var err error
			admin, err = db.OpenTenantAdmin(tenantAdminDSN)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(admin.Close)
		})

		It("creates, renames, reads and suspends a tenant", func() {
			created, err := admin.Provision(ctx, "tadm-go", "Go Co")
			Expect(err).NotTo(HaveOccurred())
			Expect(created).To(BeTrue())

			created, err = admin.Provision(ctx, "tadm-go", "Go Co Renamed")
			Expect(err).NotTo(HaveOccurred())
			Expect(created).To(BeFalse())

			rec, err := admin.Get(ctx, "tadm-go")
			Expect(err).NotTo(HaveOccurred())
			Expect(rec.DisplayName).To(Equal("Go Co Renamed"))
			Expect(rec.Status).To(Equal(db.TenantStatusActive))
			Expect(rec.CreatedAt).NotTo(BeZero())

			previous, err := admin.SetStatus(ctx, "tadm-go", db.TenantStatusSuspended)
			Expect(err).NotTo(HaveOccurred())
			Expect(previous).To(Equal(db.TenantStatusActive))

			list, err := admin.List(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(list).To(ContainElement(SatisfyAll(
				HaveField("ID", "tadm-go"),
				HaveField("DisplayName", "Go Co Renamed"),
				HaveField("Status", db.TenantStatusSuspended),
			)))
		})

		It("reports an unknown tenant as ErrTenantNotFound with the fix", func() {
			_, err := admin.Get(ctx, "tadm-nobody")
			Expect(err).To(MatchError(db.ErrTenantNotFound))
			Expect(err.Error()).To(ContainSubstring("crosscodexd admin tenant create"))

			_, err = admin.SetStatus(ctx, "tadm-nobody", db.TenantStatusSuspended)
			Expect(err).To(MatchError(db.ErrTenantNotFound))
		})

		It("imports a batch and reports created versus updated per tenant", func() {
			created, err := admin.Import(ctx, []db.TenantSpec{
				{ID: "tadm-go", DisplayName: "Go Co Imported"},
				{ID: "tadm-imp-b", DisplayName: "Imported B"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(created).To(Equal([]bool{false, true}))
			name, status, _ := tenantRow("tadm-go")
			Expect([]string{name, status}).To(Equal([]string{"Go Co Imported", "suspended"}))
		})

		It("rolls back the whole import when one tenant fails in the database", func() {
			// A graph named crosscodex_<id> without a tenants row is the #148
			// collision: inserting that tenant fails inside tenant_graph_create.
			Expect(suPool.Exec(ctx, "SELECT ag_catalog.create_graph('crosscodex_tadm-clash')")).To(Succeed())
			DeferCleanup(func() {
				Expect(suPool.Exec(ctx, "SELECT ag_catalog.drop_graph('crosscodex_tadm-clash', true)")).To(Succeed())
			})

			_, err := admin.Import(ctx, []db.TenantSpec{
				{ID: "tadm-imp-ok", DisplayName: "Would Be Created"},
				{ID: "tadm-clash", DisplayName: "Clashes"},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("tenant 2 of 2"))
			Expect(err.Error()).To(ContainSubstring("Nothing was imported"))

			_, _, found := tenantRow("tadm-imp-ok")
			Expect(found).To(BeFalse())
			Expect(graphExists("tadm-imp-ok")).To(BeFalse())
			_, _, found = tenantRow("tadm-clash")
			Expect(found).To(BeFalse())
		})
	})
})

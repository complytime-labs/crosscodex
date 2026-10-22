//go:build integration

package main

import (
	"bytes"
	"context"
	"database/sql"
	"net/url"
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
)

// withRole returns dsn with its user rewritten to role, authenticating with
// password; an empty password leaves the URL without one.
func withRole(dsn, role, password string) string {
	u, err := url.Parse(dsn)
	Expect(err).NotTo(HaveOccurred(), "parse TEST_DATABASE_DSN")
	if password == "" {
		u.User = url.User(role)
	} else {
		u.User = url.UserPassword(role, password)
	}
	return u.String()
}

// pingAs reports whether dsn authenticates.
func pingAs(ctx context.Context, dsn string) error {
	conn, err := sql.Open("pgx", dsn)
	Expect(err).NotTo(HaveOccurred())
	defer conn.Close()
	return conn.PingContext(ctx)
}

var _ = Describe("crosscodexd migrate", Ordered, func() {
	var (
		ctx     context.Context
		suDSN   string
		appPW   string
		graphPW string
	)

	BeforeAll(func() {
		suDSN = os.Getenv("TEST_DATABASE_DSN")
		if suDSN == "" {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 2*time.Minute)
		DeferCleanup(cancel)
		var err error
		// app_user keeps the run's shared password: other suites in this run
		// authenticate as app_user with it.
		appPW, err = dbtest.RolePassword(dbtest.AppUserPasswordEnv)
		Expect(err).NotTo(HaveOccurred())
		graphPW, err = dbtest.NewRolePassword()
		Expect(err).NotTo(HaveOccurred())
	})

	newConfig := func(graphPassword string) *config.Config {
		return &config.Config{
			Database: config.DatabaseConfig{
				MigrationDSN: suDSN,
				DSN:          withRole(suDSN, "app_user", appPW),
				GraphDSN:     withRole(suDSN, "graph_user", graphPassword),
			},
		}
	}

	It("migrates, sets the role passwords and provisions the default tenant, idempotently", func() {
		cfg := newConfig(graphPW)
		cfg.Tenants.DefaultTenant = "migrate-tenant"

		var out bytes.Buffer
		Expect(migrateDatabase(ctx, cfg, &out)).To(Succeed())
		Expect(out.String()).To(ContainSubstring("set the password of app_user from database.dsn"))
		Expect(out.String()).To(ContainSubstring("set the password of graph_user from database.graph_dsn"))
		Expect(out.String()).To(ContainSubstring("ensured default tenant migrate-tenant"))
		Expect(out.String()).To(MatchRegexp(`schema at version \d+`))
		Expect(out.String()).NotTo(ContainSubstring(appPW))
		Expect(out.String()).NotTo(ContainSubstring(graphPW))

		Expect(pingAs(ctx, cfg.Database.DSN)).To(Succeed(), "app_user authenticates with the password from database.dsn")
		Expect(pingAs(ctx, cfg.Database.GraphDSN)).To(Succeed(), "graph_user authenticates with the password from database.graph_dsn")

		suDB, err := sql.Open("pgx", suDSN)
		Expect(err).NotTo(HaveOccurred())
		defer suDB.Close()
		var n int
		Expect(suDB.QueryRowContext(ctx, "SELECT count(*) FROM tenants WHERE tenant_id = 'migrate-tenant'").Scan(&n)).To(Succeed())
		Expect(n).To(Equal(1))

		By("running again")
		out.Reset()
		Expect(migrateDatabase(ctx, cfg, &out)).To(Succeed())
		Expect(out.String()).To(ContainSubstring("ensured default tenant migrate-tenant"), "an existing tenant is not reported as newly provisioned")
		Expect(out.String()).NotTo(ContainSubstring("provisioned"))
		Expect(pingAs(ctx, cfg.Database.GraphDSN)).To(Succeed())
	})

	It("refuses a DSN for the wrong role and changes no password", func() {
		newGraphPW, err := dbtest.NewRolePassword()
		Expect(err).NotTo(HaveOccurred())
		cfg := newConfig(newGraphPW)
		u, err := url.Parse(suDSN)
		Expect(err).NotTo(HaveOccurred())
		cfg.Database.DSN = suDSN

		var out bytes.Buffer
		err = migrateDatabase(ctx, cfg, &out)
		Expect(err).To(MatchError(ContainSubstring(`database.dsn connects as "` + u.User.Username() + `", but it must connect as app_user`)))
		Expect(out.String()).To(BeEmpty())

		Expect(pingAs(ctx, withRole(suDSN, "graph_user", graphPW))).To(Succeed(), "graph_user's old password still authenticates")
		Expect(pingAs(ctx, withRole(suDSN, "graph_user", newGraphPW))).NotTo(Succeed(), "the failed run must not set the new password")
	})

	It("leaves the password of a role whose DSN has none", func() {
		cfg := newConfig("")

		var out bytes.Buffer
		Expect(migrateDatabase(ctx, cfg, &out)).To(Succeed())
		Expect(out.String()).To(ContainSubstring("database.graph_dsn has no password; left graph_user's password unchanged"))
		Expect(pingAs(ctx, withRole(suDSN, "graph_user", graphPW))).To(Succeed(), "graph_user's old password still authenticates")
	})
})

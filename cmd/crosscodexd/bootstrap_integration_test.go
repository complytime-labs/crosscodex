//go:build integration

package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"errors"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/jackc/pgx/v5/stdlib"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/db"
)

func TestCrosscodexdIntegrationBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Crosscodexd Integration Suite")
}

// Redirect slog output to GinkgoWriter so log noise only appears on failure.
// bootstrap() starts an internal/graph service that logs via slog.Default(),
// so without this its output would bypass Ginkgo's buffering.
var _ = BeforeEach(func() { DeferCleanup(testspecs.RedirectLogsToGinkgo()) })

// testConfig builds a minimal *config.Config pointing Database/NATS at the
// test compose stack. Database.DSN connects as the role from
// TEST_DATABASE_DSN (app_user in production, the compose superuser here, per
// the convention used by other integration suites in this repo — see
// pkg/vectordb/vectordb_integration_bdd_test.go). Database.GraphDSN is
// rewritten to authenticate as graph_user, mirroring
// pkg/graphdb/graphdb_integration_bdd_test.go, since graphdb.New requires a
// connection with graph_user's schema-ownership privileges.
func testConfig(dsn string) *config.Config {
	ctx := context.Background()

	// bootstrap() also runs migrations, but graph_user must exist (created by
	// migration 001) before we can set its password below, so migrate here
	// first; bootstrap's subsequent Up() is a documented no-op re-run.
	migrator, err := db.NewMigrator(dsn)
	Expect(err).NotTo(HaveOccurred(), "create migrator")
	if err := migrator.Up(ctx); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		migrator.Close()
		Expect(err).NotTo(HaveOccurred(), "run migrations")
	}
	migrator.Close()

	// Migrations create graph_user without a password. Set one so we can
	// authenticate as it below (idempotent: safe to run every test invocation).
	adminDB, err := sql.Open("pgx", dsn)
	Expect(err).NotTo(HaveOccurred(), "open admin connection")
	defer adminDB.Close()

	// Generate a random password per test run rather than a fixed literal.
	pwBytes := make([]byte, 16)
	_, err = rand.Read(pwBytes)
	Expect(err).NotTo(HaveOccurred(), "generate test fixture password")
	testPassword := hex.EncodeToString(pwBytes)

	// PostgreSQL's ALTER ROLE ... WITH PASSWORD does not accept a bound
	// parameter for the password literal (it's parsed as DDL, not a query
	// value) — the pgx driver returns a syntax error for $1 here. Direct
	// interpolation is safe: testPassword is always a locally-generated
	// hex string (rand.Read + hex.EncodeToString above), so it cannot
	// contain a quote or any other character requiring escaping.
	stmt := fmt.Sprintf("ALTER ROLE graph_user WITH PASSWORD '%s'", testPassword)
	_, err = adminDB.ExecContext(ctx, stmt)
	Expect(err).NotTo(HaveOccurred(), "set graph_user password")

	u, err := url.Parse(dsn)
	Expect(err).NotTo(HaveOccurred(), "parse TEST_DATABASE_DSN")
	u.User = url.UserPassword("graph_user", testPassword)
	graphDSN := u.String()

	return &config.Config{
		Database: config.DatabaseConfig{
			DSN:      dsn,
			GraphDSN: graphDSN,
		},
		NATS: config.NATSConfig{
			URL: "", // empty = embedded mode
			Embedded: config.NATSEmbeddedConfig{
				StoreDir: GinkgoT().TempDir(),
			},
		},
	}
}

var _ = Describe("bootstrap", func() {
	It("starts the graph service against a real database", func() {
		dsn := os.Getenv("TEST_DATABASE_DSN")
		if dsn == "" {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}

		cfg := testConfig(dsn)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		rt, err := bootstrap(ctx, cfg)
		Expect(err).NotTo(HaveOccurred(), "bootstrap failed")
		defer rt.close()

		Expect(rt.graphService).NotTo(BeNil(), "bootstrap did not construct a graph service")

		Expect(rt.graphService.Start(ctx)).To(Succeed(), "graph service Start failed")
		defer func() {
			Expect(rt.graphService.Stop(ctx)).To(Succeed(), "graph service Stop failed")
		}()
	})

	It("fails fast on an unreachable graph_user DSN", func() {
		dsn := os.Getenv("TEST_DATABASE_DSN")
		if dsn == "" {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}

		cfg := testConfig(dsn)
		// Point GraphDSN at a port nothing is listening on, so PingContext fails
		// fast instead of the daemon reporting a healthy bootstrap.
		u, err := url.Parse(cfg.Database.GraphDSN)
		Expect(err).NotTo(HaveOccurred(), "parse GraphDSN")
		u.Host = "127.0.0.1:1"
		cfg.Database.GraphDSN = u.String()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		_, err = bootstrap(ctx, cfg)
		Expect(err).To(HaveOccurred(), "expected bootstrap to fail on an unreachable graph_user DSN")
		Expect(err.Error()).To(ContainSubstring("ping graph_user sql.DB"))
	})
})

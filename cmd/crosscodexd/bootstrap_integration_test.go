//go:build integration

package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/testcerts"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
)

// testAttestationPrivateKeyPath and testAttestationPublicKeyPath point at a
// throwaway ECDSA P-256 keypair generated once per suite run (see the
// BeforeSuite below) and written out as PEM files, matching the on-disk
// format pkg/attestation.FileKeyProvider expects. Gateway-role tests that
// need a valid attestation configuration reference these paths instead of
// each generating their own key.
var (
	testAttestationPrivateKeyPath string
	testAttestationPublicKeyPath  string
)

// testTLS* point at a single throwaway mTLS PKI bundle generated once per
// suite run (see BeforeSuite) via internal/testcerts. The standalone
// pipeline role now fails closed unless it resolves a mutual-TLS config with
// a CA (attachPipeline in bootstrap.go), so every role=pipeline test that is
// expected to get past that check references these paths. testServerTLS and
// testClientTLS assemble the server-side and client-side config.TLSConfig
// from them.
// testGraphUserPassword is graph_user's password for this suite, generated
// once in BeforeSuite so every config built by baseTestConfig agrees on it:
// each call re-sets it, and a fresh value per call would lock out a runtime
// built from an earlier config.
var testGraphUserPassword string

var (
	testTLSCAPath         string
	testTLSServerCertPath string
	testTLSServerKeyPath  string
	testTLSClientCertPath string
	testTLSClientKeyPath  string
)

// testServerTLS returns a mutual-TLS server config (CA + server cert/key)
// for role=pipeline tests that bootstrap a real pipeline listener.
func testServerTLS() config.TLSConfig {
	return config.TLSConfig{
		Mode: "mutual",
		CA:   testTLSCAPath,
		Cert: testTLSServerCertPath,
		Key:  testTLSServerKeyPath,
	}
}

// testClientTLS returns a mutual-TLS client config (CA + client cert/key)
// for tests that dial a real pipeline listener with a client identity.
func testClientTLS() config.TLSConfig {
	return config.TLSConfig{
		Mode: "mutual",
		CA:   testTLSCAPath,
		Cert: testTLSClientCertPath,
		Key:  testTLSClientKeyPath,
	}
}

// BeforeSuite generates the shared test attestation keypair once, mirroring
// the key-generation code in
// internal/pipeline/pipeline_e2e_integration_test.go's e2eKeyProvider, but
// writing the key material to disk (PEM-encoded) since
// attestation.FileKeyProvider reads keys from files rather than accepting
// them in-memory.
var _ = BeforeSuite(func() {
	dir := GinkgoT().TempDir()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	Expect(err).NotTo(HaveOccurred(), "generate test attestation key")

	privDER, err := x509.MarshalECPrivateKey(key)
	Expect(err).NotTo(HaveOccurred(), "marshal test attestation private key")
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privDER})

	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	Expect(err).NotTo(HaveOccurred(), "marshal test attestation public key")
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})

	testAttestationPrivateKeyPath = filepath.Join(dir, "attestation-private.pem")
	testAttestationPublicKeyPath = filepath.Join(dir, "attestation-public.pem")

	Expect(os.WriteFile(testAttestationPrivateKeyPath, privPEM, 0o600)).
		To(Succeed(), "write test attestation private key")
	Expect(os.WriteFile(testAttestationPublicKeyPath, pubPEM, 0o644)).
		To(Succeed(), "write test attestation public key")

	// Generate one mTLS PKI bundle for the whole suite, mirroring the
	// once-per-suite attestation keypair above. internal/testcerts wraps
	// pkg/tlsconfig/pki and is the repo's standard test-cert tooling.
	certDir := GinkgoT().TempDir()
	pki, err := testcerts.Generate()
	Expect(err).NotTo(HaveOccurred(), "generate test PKI")
	Expect(pki.WriteToDir(certDir)).To(Succeed(), "write test PKI")
	testTLSCAPath = filepath.Join(certDir, "ca.pem")
	testTLSServerCertPath = filepath.Join(certDir, "server.pem")
	testTLSServerKeyPath = filepath.Join(certDir, "server-key.pem")
	testTLSClientCertPath = filepath.Join(certDir, "client.pem")
	testTLSClientKeyPath = filepath.Join(certDir, "client-key.pem")

	testGraphUserPassword, err = dbtest.NewRolePassword()
	Expect(err).NotTo(HaveOccurred(), "generate graph_user password")
})

func TestCrosscodexdIntegrationBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Crosscodexd Integration Suite")
}

// Redirect slog output to GinkgoWriter so log noise only appears on failure.
// bootstrap() starts an internal/graph service that logs via slog.Default(),
// so without this its output would bypass Ginkgo's buffering.
var _ = BeforeEach(func() { DeferCleanup(testspecs.RedirectLogsToGinkgo()) })

// baseTestConfig builds a minimal *config.Config pointing Database/NATS at
// the test compose stack, with no Role set — callers set cfg.Role to the
// role under test. suDSN is TEST_DATABASE_DSN, the compose superuser, used
// only as database.migration_dsn: the daemon refuses a superuser, so
// Database.DSN connects as app_user (with the run's shared
// TEST_APP_USER_PASSWORD, which other suites also use) and Database.GraphDSN
// as graph_user, as in production. Database.Extensions mirrors the
// production default (see config.defaults) so buildSharedResources'
// VerifyExtensions call has something to check.
//
// It runs migrateDatabase (crosscodexd migrate), which migrates the schema
// and sets both role passwords. A role=all spec must set
// cfg.Tenants.DefaultTenant and call migrateDatabase again to provision the
// tenant, because the daemon only checks that it exists.
func baseTestConfig(suDSN string) *config.Config {
	appPassword, err := dbtest.RolePassword(dbtest.AppUserPasswordEnv)
	Expect(err).NotTo(HaveOccurred())
	cfg := &config.Config{
		Database: config.DatabaseConfig{
			MigrationDSN: suDSN,
			DSN:          withRole(suDSN, "app_user", appPassword),
			GraphDSN:     withRole(suDSN, "graph_user", testGraphUserPassword),
			Extensions:   []string{"age", "vector"},
		},
		NATS: config.NATSConfig{
			URL: "", // empty = embedded mode
			Embedded: config.NATSEmbeddedConfig{
				StoreDir: GinkgoT().TempDir(),
			},
		},
	}
	Expect(migrateDatabase(context.Background(), cfg, GinkgoWriter)).To(Succeed(), "crosscodexd migrate")
	return cfg
}

// testConfig is baseTestConfig for the graph role.
func testConfig(suDSN string) *config.Config {
	cfg := baseTestConfig(suDSN)
	cfg.Role = config.RoleGraph
	return cfg
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

var _ = Describe("bootstrap with role=worker", func() {
	It("builds a worker service and no database resources", func() {
		dsn := os.Getenv("TEST_DATABASE_DSN")
		if dsn == "" {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}

		cfg := baseTestConfig(dsn)
		cfg.Role = config.RoleWorker
		// GatewayURL only needs to be non-empty: llmclient.NewClient stores it
		// without dialing out, so no live LLM gateway is required here.
		cfg.LLM = config.LLMConfig{GatewayURL: "http://127.0.0.1:0", DefaultModel: "test-model", EmbeddingModel: "test-model"}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		rt, err := bootstrap(ctx, cfg)
		Expect(err).NotTo(HaveOccurred(), "bootstrap failed")
		defer rt.close()

		Expect(rt.workerService).NotTo(BeNil(), "bootstrap did not construct a worker service")
		Expect(rt.graphService).To(BeNil(), "worker role must not construct a graph service")
		Expect(rt.shared.appPool).To(BeNil(), "worker role must never open a database pool")
	})
})

var _ = Describe("bootstrap with role=gateway", func() {
	It("builds a gateway server whose pipeline backend is a network client, not an in-process service", func() {
		dsn := os.Getenv("TEST_DATABASE_DSN")
		if dsn == "" {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}

		cfg := baseTestConfig(dsn)
		cfg.Role = config.RoleGateway
		cfg.Server.Addr = "127.0.0.1:0"
		cfg.Pipeline.Endpoint = "127.0.0.1:0" // construction only -- no call is made in this test
		// attachGateway now requires a mutual-TLS pipeline-client identity to
		// reach a standalone pipeline role; without it, construction fails closed.
		cfg.TLS = testClientTLS()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		rt, err := bootstrap(ctx, cfg)
		Expect(err).NotTo(HaveOccurred(), "bootstrap failed")
		defer rt.close()

		Expect(rt.gatewayServer).NotTo(BeNil(), "bootstrap did not construct a gateway server")
		Expect(rt.pipelineService).To(BeNil(), "a standalone gateway role must not build an in-process pipeline.Service")
		Expect(rt.pipelineServer).To(BeNil(), "a standalone gateway role must not own a pipeline listener")
	})

	It("fails closed when pipeline.endpoint is not configured", func() {
		dsn := os.Getenv("TEST_DATABASE_DSN")
		if dsn == "" {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}

		cfg := baseTestConfig(dsn)
		cfg.Role = config.RoleGateway
		cfg.Server.Addr = "127.0.0.1:0"
		cfg.Pipeline.Endpoint = ""

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		_, err := bootstrap(ctx, cfg)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("pipeline.endpoint"))
	})

	It("fails closed when pipeline.endpoint is set but TLS is not mutual", func() {
		dsn := os.Getenv("TEST_DATABASE_DSN")
		if dsn == "" {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}

		cfg := baseTestConfig(dsn)
		cfg.Role = config.RoleGateway
		cfg.Server.Addr = "127.0.0.1:0"
		cfg.Pipeline.Endpoint = "127.0.0.1:0"
		// No cfg.TLS: the standalone pipeline listener enforces mTLS with no
		// plaintext fallback, so a gateway that can't present a client cert must
		// fail closed at construction rather than only at the first RPC. This is
		// the client-side mirror of attachPipeline's server-side precondition.

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		_, err := bootstrap(ctx, cfg)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("mutual TLS"))
	})
})

var _ = Describe("bootstrap with role=pipeline", func() {
	It("builds a pipeline service and a standalone pipeline server", func() {
		dsn := os.Getenv("TEST_DATABASE_DSN")
		if dsn == "" {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}

		cfg := baseTestConfig(dsn)
		cfg.Role = config.RolePipeline
		cfg.NATS = config.NATSConfig{Embedded: config.NATSEmbeddedConfig{StoreDir: GinkgoT().TempDir()}}
		cfg.LLM = config.LLMConfig{GatewayURL: "http://127.0.0.1:0", DefaultModel: "test-model", EmbeddingModel: "test-model"}
		cfg.Pipeline.Addr = "127.0.0.1:0"
		cfg.Tenants.DefaultTenant = "crosscodexd-test"
		cfg.Attestation.PrivateKeyPath = testAttestationPrivateKeyPath
		cfg.Attestation.PublicKeyPath = testAttestationPublicKeyPath
		cfg.Storage.Objects.BasePath = GinkgoT().TempDir()
		// attachPipeline now fails closed unless mutual TLS is configured.
		cfg.TLS = testServerTLS()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		rt, err := bootstrap(ctx, cfg)
		Expect(err).NotTo(HaveOccurred(), "bootstrap failed")
		defer rt.close()

		Expect(rt.pipelineService).NotTo(BeNil(), "bootstrap did not construct a pipeline service")
		Expect(rt.pipelineServer).NotTo(BeNil(), "bootstrap did not construct a pipeline server")
		Expect(rt.gatewayServer).To(BeNil(), "pipeline role must not construct a gateway server")
	})

	It("fails closed when pipeline.addr is not configured", func() {
		dsn := os.Getenv("TEST_DATABASE_DSN")
		if dsn == "" {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}

		cfg := baseTestConfig(dsn)
		cfg.Role = config.RolePipeline
		cfg.NATS = config.NATSConfig{Embedded: config.NATSEmbeddedConfig{StoreDir: GinkgoT().TempDir()}}
		cfg.LLM = config.LLMConfig{GatewayURL: "http://127.0.0.1:0", DefaultModel: "test-model", EmbeddingModel: "test-model"}
		cfg.Pipeline.Addr = ""
		cfg.Tenants.DefaultTenant = "crosscodexd-test"
		cfg.Attestation.PrivateKeyPath = testAttestationPrivateKeyPath
		cfg.Attestation.PublicKeyPath = testAttestationPublicKeyPath
		cfg.Storage.Objects.BasePath = GinkgoT().TempDir()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		_, err := bootstrap(ctx, cfg)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("pipeline.addr"))
	})

	It("fails closed when attestation key paths are not configured", func() {
		dsn := os.Getenv("TEST_DATABASE_DSN")
		if dsn == "" {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}

		cfg := baseTestConfig(dsn)
		cfg.Role = config.RolePipeline
		cfg.NATS = config.NATSConfig{Embedded: config.NATSEmbeddedConfig{StoreDir: GinkgoT().TempDir()}}
		cfg.LLM = config.LLMConfig{GatewayURL: "http://127.0.0.1:0", DefaultModel: "test-model", EmbeddingModel: "test-model"}
		cfg.Pipeline.Addr = "127.0.0.1:0"
		cfg.Tenants.DefaultTenant = "crosscodexd-test"
		cfg.Attestation.PrivateKeyPath = ""
		cfg.Attestation.PublicKeyPath = ""
		// Valid TLS so this test fails on the attestation precondition, not
		// attachPipeline's new mutual-TLS check.
		cfg.TLS = testServerTLS()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		_, err := bootstrap(ctx, cfg)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("attestation"))
	})

	It("fails closed when no default tenant is configured", func() {
		dsn := os.Getenv("TEST_DATABASE_DSN")
		if dsn == "" {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}

		cfg := baseTestConfig(dsn)
		cfg.Role = config.RolePipeline
		cfg.NATS = config.NATSConfig{Embedded: config.NATSEmbeddedConfig{StoreDir: GinkgoT().TempDir()}}
		cfg.LLM = config.LLMConfig{GatewayURL: "http://127.0.0.1:0", DefaultModel: "test-model", EmbeddingModel: "test-model"}
		cfg.Pipeline.Addr = "127.0.0.1:0"
		cfg.Attestation.PrivateKeyPath = testAttestationPrivateKeyPath
		cfg.Attestation.PublicKeyPath = testAttestationPublicKeyPath
		cfg.Tenants.DefaultTenant = ""
		// Valid TLS so this test fails on the tenant precondition, not
		// attachPipeline's new mutual-TLS check.
		cfg.TLS = testServerTLS()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		_, err := bootstrap(ctx, cfg)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("tenants.default_tenant"))
	})

	It("fails closed when no storage base path is configured", func() {
		dsn := os.Getenv("TEST_DATABASE_DSN")
		if dsn == "" {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}

		cfg := baseTestConfig(dsn)
		cfg.Role = config.RolePipeline
		cfg.NATS = config.NATSConfig{Embedded: config.NATSEmbeddedConfig{StoreDir: GinkgoT().TempDir()}}
		cfg.LLM = config.LLMConfig{GatewayURL: "http://127.0.0.1:0", DefaultModel: "test-model", EmbeddingModel: "test-model"}
		cfg.Pipeline.Addr = "127.0.0.1:0"
		cfg.Tenants.DefaultTenant = "crosscodexd-test"
		cfg.Attestation.PrivateKeyPath = testAttestationPrivateKeyPath
		cfg.Attestation.PublicKeyPath = testAttestationPublicKeyPath
		cfg.Storage.Objects.BasePath = ""
		// Valid TLS so this test fails on the storage precondition, not
		// attachPipeline's new mutual-TLS check.
		cfg.TLS = testServerTLS()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		_, err := bootstrap(ctx, cfg)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("storage.objects.base_path"))
	})
})

var _ = Describe("bootstrap with role=all", func() {
	It("builds every service sharing one set of resources", func() {
		dsn := os.Getenv("TEST_DATABASE_DSN")
		if dsn == "" {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}

		cfg := baseTestConfig(dsn)
		cfg.Role = config.RoleAll
		cfg.NATS = config.NATSConfig{Embedded: config.NATSEmbeddedConfig{StoreDir: GinkgoT().TempDir()}}
		cfg.LLM = config.LLMConfig{GatewayURL: "http://127.0.0.1:0", DefaultModel: "test-model", EmbeddingModel: "test-model"}
		cfg.Server.Addr = "127.0.0.1:0"
		cfg.Tenants.DefaultTenant = "crosscodexd-test"
		cfg.Attestation.PrivateKeyPath = testAttestationPrivateKeyPath
		cfg.Attestation.PublicKeyPath = testAttestationPublicKeyPath
		cfg.Storage.Objects.BasePath = GinkgoT().TempDir()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		Expect(migrateDatabase(ctx, cfg, GinkgoWriter)).To(Succeed(), "provision the default tenant")

		rt, err := bootstrap(ctx, cfg)
		Expect(err).NotTo(HaveOccurred(), "bootstrap failed")
		defer rt.close()

		Expect(rt.graphService).NotTo(BeNil(), "bootstrap did not construct a graph service")
		Expect(rt.workerService).NotTo(BeNil(), "bootstrap did not construct a worker service")
		Expect(rt.pipelineService).NotTo(BeNil(), "bootstrap did not construct a pipeline service")
		Expect(rt.gatewayServer).NotTo(BeNil(), "bootstrap did not construct a gateway server")
		// "all" relies on the gateway's own /healthz; no separate
		// dedicated health listener is started for co-located services.
		Expect(rt.healthServer).To(BeNil(), "all role must not leave a dedicated health listener up")
	})
})

// The daemon connects only as the restricted roles, which RLS applies to, and
// never migrates: crosscodexd migrate does both owner-only jobs. Each spec
// checks the refusal names what to do, and that the database is unchanged.
var _ = Describe("buildSharedResources refuses an unrestricted or unmigrated database", func() {
	var (
		ctx   context.Context
		suDSN string
		suDB  *sql.DB
	)

	BeforeEach(func() {
		suDSN = os.Getenv("TEST_DATABASE_DSN")
		if suDSN == "" {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), time.Minute)
		DeferCleanup(cancel)
		var err error
		suDB, err = sql.Open("pgx", suDSN)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(suDB.Close)
	})

	type schemaVersion struct {
		version int64
		dirty   bool
	}
	schemaState := func() schemaVersion {
		var s schemaVersion
		Expect(suDB.QueryRowContext(ctx, "SELECT version, dirty FROM schema_migrations").Scan(&s.version, &s.dirty)).To(Succeed())
		return s
	}
	superuser := func() string {
		u, err := url.Parse(suDSN)
		Expect(err).NotTo(HaveOccurred())
		return u.User.Username()
	}

	It("connects as the restricted roles without migrating", func() {
		cfg := baseTestConfig(suDSN)
		before := schemaState()

		res, err := buildSharedResources(ctx, cfg, requiredResources{db: true, graph: true})
		Expect(err).NotTo(HaveOccurred())
		res.close()

		Expect(schemaState()).To(Equal(before))
	})

	It("refuses a superuser database.dsn", func() {
		cfg := baseTestConfig(suDSN)
		cfg.Database.DSN = suDSN
		before := schemaState()

		_, err := buildSharedResources(ctx, cfg, requiredResources{db: true})
		Expect(err).To(MatchError(ContainSubstring(`database.dsn connects as "` + superuser() + `", which is a superuser`)))
		Expect(err.Error()).To(ContainSubstring("crosscodexd migrate"))
		Expect(schemaState()).To(Equal(before))
	})

	It("refuses a superuser database.graph_dsn", func() {
		cfg := baseTestConfig(suDSN)
		cfg.Database.GraphDSN = suDSN

		_, err := buildSharedResources(ctx, cfg, requiredResources{db: true, graph: true})
		Expect(err).To(MatchError(ContainSubstring(`database.graph_dsn connects as "` + superuser() + `", which is a superuser`)))
		Expect(err.Error()).To(ContainSubstring("connect database.graph_dsn as graph_user"))
	})

	It("refuses a superuser database.purge_dsn", func() {
		cfg := baseTestConfig(suDSN)
		cfg.Database.PurgeDSN = suDSN
		res, err := buildSharedResources(ctx, cfg, requiredResources{db: true})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(res.close)
		var rowsBefore int
		Expect(suDB.QueryRowContext(ctx, "SELECT count(*) FROM jobs").Scan(&rowsBefore)).To(Succeed())

		_, err = buildRetentionWiring(ctx, cfg, res, slog.Default())
		Expect(err).To(MatchError(ContainSubstring(`database.purge_dsn connects as "` + superuser() + `", which is a superuser`)))
		Expect(err.Error()).To(ContainSubstring("connect database.purge_dsn as purge_user"))
		var rowsAfter int
		Expect(suDB.QueryRowContext(ctx, "SELECT count(*) FROM jobs").Scan(&rowsAfter)).To(Succeed())
		Expect(rowsAfter).To(Equal(rowsBefore), "nothing may be purged")
	})

	It("accepts purge_user as database.purge_dsn", func() {
		purgePassword, err := dbtest.RolePassword(dbtest.PurgeUserPasswordEnv)
		Expect(err).NotTo(HaveOccurred())
		cfg := baseTestConfig(suDSN)
		cfg.Database.PurgeDSN = withRole(suDSN, "purge_user", purgePassword)
		Expect(migrateDatabase(ctx, cfg, GinkgoWriter)).To(Succeed(), "set purge_user's password")
		res, err := buildSharedResources(ctx, cfg, requiredResources{db: true})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(res.close)

		wiring, err := buildRetentionWiring(ctx, cfg, res, slog.Default())
		Expect(err).NotTo(HaveOccurred())
		Expect(wiring.purgePool).NotTo(BeNil())
		Expect(wiring.purgePool.Close()).To(Succeed())
	})

	It("refuses a schema it cannot read the version of", func() {
		cfg := baseTestConfig(suDSN)
		_, err := suDB.ExecContext(ctx, "REVOKE SELECT ON schema_migrations FROM app_user")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_, err := suDB.ExecContext(context.Background(), "GRANT SELECT ON schema_migrations TO app_user")
			Expect(err).NotTo(HaveOccurred(), "restore app_user's SELECT on schema_migrations")
		})
		before := schemaState()

		_, err = buildSharedResources(ctx, cfg, requiredResources{db: true})
		Expect(err).To(MatchError(ContainSubstring("Run `crosscodexd migrate`")))
		Expect(schemaState()).To(Equal(before))
	})

	// The schema has a single migration, so the only state behind it is the
	// one a full `migrate down` leaves: no version row at all.
	It("refuses a schema behind this binary instead of migrating it", func() {
		cfg := baseTestConfig(suDSN)
		before := schemaState()
		_, err := suDB.ExecContext(ctx, "DELETE FROM schema_migrations")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_, err := suDB.ExecContext(context.Background(),
				"INSERT INTO schema_migrations (version, dirty) VALUES ($1, $2)", before.version, before.dirty)
			Expect(err).NotTo(HaveOccurred(), "restore the schema version")
		})

		_, err = buildSharedResources(ctx, cfg, requiredResources{db: true})
		Expect(err).To(MatchError(ContainSubstring("Run `crosscodexd migrate`")))
		var rows int
		Expect(suDB.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations").Scan(&rows)).To(Succeed())
		Expect(rows).To(BeZero(), "the daemon must not migrate")
	})

	It("refuses role all when the default tenant is deleted and says how to restore it", func() {
		const tenantID = "deleted-default"
		hardDelete, err := dbtest.HardDeleteTenantSQL(tenantID)
		Expect(err).NotTo(HaveOccurred())
		_, err = suDB.ExecContext(ctx, hardDelete)
		Expect(err).NotTo(HaveOccurred(), "remove a leftover tenant row")
		DeferCleanup(func() {
			_, err := suDB.ExecContext(context.Background(), hardDelete)
			Expect(err).NotTo(HaveOccurred(), "remove the test tenant")
		})

		cfg := baseTestConfig(suDSN)
		cfg.Role = config.RoleAll
		cfg.LLM = config.LLMConfig{GatewayURL: "http://127.0.0.1:0", DefaultModel: "test-model", EmbeddingModel: "test-model"}
		cfg.Server.Addr = "127.0.0.1:0"
		cfg.Tenants.DefaultTenant = tenantID
		cfg.Attestation.PrivateKeyPath = testAttestationPrivateKeyPath
		cfg.Attestation.PublicKeyPath = testAttestationPublicKeyPath
		cfg.Storage.Objects.BasePath = GinkgoT().TempDir()
		Expect(migrateDatabase(ctx, cfg, GinkgoWriter)).To(Succeed(), "provision the default tenant")
		// A superuser UPDATE soft-deletes it: delete_tenant needs a signed
		// attestation, and the row state is all enter_tenant reads.
		_, err = suDB.ExecContext(ctx, "UPDATE tenants SET status = 'deleted', deleted_at = now(), deleted_by = 'test' WHERE tenant_id = $1", tenantID)
		Expect(err).NotTo(HaveOccurred())

		_, err = bootstrap(ctx, cfg)
		Expect(err).To(MatchError(ContainSubstring(`default tenant "deleted-default" is deleted. Restore it with crosscodexd admin tenant restore --tenant deleted-default --by <operator> --reason <text>, or set tenants.default_tenant to an active tenant`)))

		var status string
		Expect(suDB.QueryRowContext(ctx, "SELECT status FROM tenants WHERE tenant_id = $1", tenantID).Scan(&status)).To(Succeed())
		Expect(status).To(Equal("deleted"), "the daemon must not restore the tenant")
	})

	It("refuses role all when the default tenant was never provisioned", func() {
		const tenantID = "never-provisioned"
		hardDelete, err := dbtest.HardDeleteTenantSQL(tenantID)
		Expect(err).NotTo(HaveOccurred())
		_, err = suDB.ExecContext(ctx, hardDelete)
		Expect(err).NotTo(HaveOccurred(), "remove a leftover tenant row")

		cfg := baseTestConfig(suDSN)
		cfg.Role = config.RoleAll
		cfg.LLM = config.LLMConfig{GatewayURL: "http://127.0.0.1:0", DefaultModel: "test-model", EmbeddingModel: "test-model"}
		cfg.Server.Addr = "127.0.0.1:0"
		cfg.Tenants.DefaultTenant = tenantID
		cfg.Attestation.PrivateKeyPath = testAttestationPrivateKeyPath
		cfg.Attestation.PublicKeyPath = testAttestationPublicKeyPath
		cfg.Storage.Objects.BasePath = GinkgoT().TempDir()

		_, err = bootstrap(ctx, cfg)
		Expect(err).To(MatchError(ContainSubstring(`default tenant "never-provisioned" does not exist. Run ` + "`crosscodexd migrate`")))

		var n int
		Expect(suDB.QueryRowContext(ctx, "SELECT count(*) FROM tenants WHERE tenant_id = $1", tenantID).Scan(&n)).To(Succeed())
		Expect(n).To(BeZero(), "the daemon must not provision the tenant")
	})
})

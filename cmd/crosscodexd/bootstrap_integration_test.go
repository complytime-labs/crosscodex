//go:build integration

package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"errors"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/jackc/pgx/v5/stdlib"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/testcerts"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/db"
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
// role under test. Database.DSN connects as the role from TEST_DATABASE_DSN
// (app_user in production, the compose superuser here, per the convention
// used by other integration suites in this repo — see
// pkg/vectordb/vectordb_integration_bdd_test.go). Database.Extensions
// mirrors the production default (see config.defaults) so buildSharedResources'
// VerifyExtensions call has something to check.
//
// This helper does not set Database.GraphDSN — the graph_user credential it
// requires is only needed by roles that request the graph resource, so
// graph-role tests derive their own GraphDSN via testConfig below rather
// than paying that setup cost unconditionally.
func baseTestConfig(dsn string) *config.Config {
	return &config.Config{
		Database: config.DatabaseConfig{
			DSN:        dsn,
			GraphDSN:   dsn,
			Extensions: []string{"age", "vector"},
		},
		NATS: config.NATSConfig{
			URL: "", // empty = embedded mode
			Embedded: config.NATSEmbeddedConfig{
				StoreDir: GinkgoT().TempDir(),
			},
		},
	}
}

// provisionGraphUser runs migrations and assigns graph_user a fresh random
// password, returning a DSN authenticated as graph_user. graph_user (created
// by migration 001) must exist and own the per-tenant graph schemas before
// graphdb.New can connect; both RoleGraph and RoleAll require this.
func provisionGraphUser(ctx context.Context, dsn string) string {
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
	return u.String()
}

// testConfig builds on baseTestConfig for the graph role: it runs migrations
// up front (graph_user must exist, created by migration 001, before we can
// set its password below) and rewrites Database.GraphDSN to authenticate as
// graph_user, mirroring pkg/graphdb/graphdb_integration_bdd_test.go, since
// graphdb.New requires a connection with graph_user's schema-ownership
// privileges.
func testConfig(dsn string) *config.Config {
	graphDSN := provisionGraphUser(context.Background(), dsn)
	cfg := baseTestConfig(dsn)
	cfg.Database.GraphDSN = graphDSN
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

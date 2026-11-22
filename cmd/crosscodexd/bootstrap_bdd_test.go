//go:build !integration

package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/graph"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/llmclient/llmclienttest"
	"github.com/complytime-labs/crosscodex/pkg/natsbus/natsbustest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
	pkigen "github.com/complytime-labs/crosscodex/pkg/tlsconfig/pki"
	"github.com/complytime-labs/crosscodex/pkg/vectordb/vectordbtest"
)

// attachAllGateway delegates its config guards to buildIngestGateway (specified
// below); this spec only proves it propagates the builder's refusal. It calls
// attachAllGateway directly rather than through bootstrap(): for role=all,
// buildPipelineService validates the same field first and would mask it.
// The zero graph.Service only gets past the graph-service guard, which runs
// first; the builder refuses before the service is used.
var _ = Describe("attachAllGateway config guards", func() {
	It("propagates buildIngestGateway's refusal of a missing default tenant", func() {
		cfg := &config.Config{}
		cfg.Storage.Objects.BasePath = "/tmp/does-not-matter"

		err := attachAllGateway(context.Background(), cfg, &sharedResources{}, &runtime{graphService: &graph.Service{}}, nil)
		Expect(err).To(MatchError("all role: tenants.default_tenant must be set"))
	})

	It("fails closed before building anything when the graph service is not built", func() {
		cfg := &config.Config{}
		cfg.Tenants.DefaultTenant = "crosscodexd-test"
		cfg.Storage.Objects.BasePath = "/tmp/does-not-matter"

		err := attachAllGateway(context.Background(), cfg, &sharedResources{}, &runtime{}, nil)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("graph service is not built"))
	})
})

// buildIngestGateway serves catalog import for both gateway-serving roles, so
// each refusal names the role whose configuration is incomplete. Both guards
// run before the default-tenant database check, so no shared resource is
// needed to reach them.
var _ = Describe("buildIngestGateway config guards", func() {
	DescribeTable("fails closed, naming the role, when a required setting is empty",
		func(role, defaultTenant, backend, basePath, want string) {
			cfg := &config.Config{}
			cfg.Tenants.DefaultTenant = defaultTenant
			cfg.Storage.Objects.Backend = backend
			cfg.Storage.Objects.BasePath = basePath

			svc, err := buildIngestGateway(context.Background(), role, cfg, &sharedResources{}, nil, nil)
			Expect(svc).To(BeNil())
			Expect(err).To(MatchError(want))
		},
		Entry("gateway role without a default tenant",
			config.RoleGateway, "", "local", "/tmp/does-not-matter", "gateway role: tenants.default_tenant must be set"),
		Entry("gateway role, local backend without a storage base path",
			config.RoleGateway, "crosscodexd-test", "local", "", "gateway role: storage.objects.base_path must be set"),
		Entry("gateway role without a storage backend",
			config.RoleGateway, "crosscodexd-test", "", "/tmp/does-not-matter",
			`gateway role: storage.objects.backend "" is not supported. Set it to "local" or "s3"`),
		Entry("all role without a default tenant",
			config.RoleAll, "", "local", "/tmp/does-not-matter", "all role: tenants.default_tenant must be set"),
		Entry("all role, local backend without a storage base path",
			config.RoleAll, "crosscodexd-test", "local", "", "all role: storage.objects.base_path must be set"),
		Entry("all role without a storage backend",
			config.RoleAll, "crosscodexd-test", "", "/tmp/does-not-matter",
			`all role: storage.objects.backend "" is not supported. Set it to "local" or "s3"`),
	)

	// The s3 backend keeps objects in a bucket, so base_path does not apply:
	// the builder gets past the storage guard to the TLS guard after it.
	DescribeTable("does not require a storage base path for the s3 backend",
		func(role string) {
			cfg := &config.Config{}
			cfg.Tenants.DefaultTenant = "crosscodexd-test"
			cfg.Storage.Objects = config.ObjectStorageConfig{Backend: "s3", Bucket: "crosscodex-objects"}

			svc, err := buildIngestGateway(context.Background(), role, cfg, &sharedResources{}, nil, nil)
			Expect(svc).To(BeNil())
			Expect(err).To(MatchError(ContainSubstring("catalog import authenticates clients by X.509 certificate")))
		},
		Entry("gateway role", config.RoleGateway),
		Entry("all role", config.RoleAll),
	)
})

// Catalog import signs an ingest attestation for every import, so the
// gateway-serving roles refuse to start, before any database access, when
// attestation is enabled without a key pair.
var _ = Describe("buildIngestGateway attestation guard", func() {
	DescribeTable("refuses attestation enabled without a key pair, naming the role, the setting and both remedies",
		func(role string) {
			cfg := &config.Config{}
			cfg.Tenants.DefaultTenant = "crosscodexd-test"
			cfg.Storage.Objects = config.ObjectStorageConfig{Backend: "local", BasePath: GinkgoT().TempDir()}
			cfg.Attestation = config.AttestationConfig{Enabled: true, ExpiryDuration: time.Hour}

			svc, err := buildIngestGateway(context.Background(), role, cfg, &sharedResources{}, nil, nil)
			Expect(svc).To(BeNil())
			Expect(err).To(MatchError(config.ErrMissingRequired))
			Expect(err.Error()).To(And(HavePrefix(role+" role: "),
				ContainSubstring("attestation.private_key_path"), ContainSubstring("attestation.enabled: false")))
		},
		Entry("gateway role", config.RoleGateway),
		Entry("all role", config.RoleAll),
	)
})

// objectStore is the daemon's object storage for every role that keeps
// objects: it opens storage.objects for the tenant on each call's context,
// never for tenants.default_tenant alone.
var _ = Describe("objectStore", func() {
	const key = "uploads/doc-1.json"
	tenantCtx := func(id string) context.Context {
		ctx, err := tenant.WithTenant(context.Background(), id)
		Expect(err).NotTo(HaveOccurred())
		return ctx
	}
	localConfig := func() *config.Config {
		cfg := &config.Config{}
		cfg.Tenants.DefaultTenant = "crosscodexd-test"
		cfg.Storage.Objects = config.ObjectStorageConfig{Backend: "local", BasePath: GinkgoT().TempDir()}
		return cfg
	}

	It("stores a tenant's object under that tenant's directory of base_path, not the default tenant's", func() {
		cfg := localConfig()
		store := objectStore(cfg)
		DeferCleanup(store.Close)

		Expect(store.Put(tenantCtx("t-b"), key, strings.NewReader("payload"))).To(Succeed())

		got, err := os.ReadFile(filepath.Join(cfg.Storage.Objects.BasePath, "t-b", key))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(got)).To(Equal("payload"))
		Expect(filepath.Join(cfg.Storage.Objects.BasePath, cfg.Tenants.DefaultTenant, key)).NotTo(BeAnExistingFile())
	})

	It("refuses a call without a tenant on its context and writes nothing", func() {
		cfg := localConfig()
		store := objectStore(cfg)
		DeferCleanup(store.Close)

		err := store.Put(context.Background(), key, strings.NewReader("payload"))
		Expect(err).To(MatchError(tenant.ErrNoTenant))
		Expect(err.Error()).To(ContainSubstring("tenant.WithTenant"))
		entries, err := os.ReadDir(cfg.Storage.Objects.BasePath)
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(BeEmpty())
	})

	// Static credentials and a disabled instance-metadata lookup keep the
	// SDK off the network; the only address it dials is the endpoint, where
	// nothing listens.
	It("sends a tenant's object to the configured s3 endpoint", func() {
		GinkgoT().Setenv("AWS_ACCESS_KEY_ID", "test-access-key")
		GinkgoT().Setenv("AWS_SECRET_ACCESS_KEY", "test-secret-key")
		GinkgoT().Setenv("AWS_EC2_METADATA_DISABLED", "true")
		cfg := &config.Config{}
		cfg.Storage.Objects = config.ObjectStorageConfig{
			Backend: "s3", Bucket: "crosscodex-objects", Region: "us-east-1", Endpoint: "http://127.0.0.1:1",
		}
		store := objectStore(cfg)
		DeferCleanup(store.Close)

		err := store.Put(tenantCtx("t-b"), key, strings.NewReader("payload"))
		Expect(err).To(MatchError(ContainSubstring("127.0.0.1:1")))
	})
})

// buildPipelineService opens object storage lazily, per tenant, so building
// it with the s3 backend contacts no endpoint; a base_path check applies only
// to the local backend.
var _ = Describe("buildPipelineService object storage", func() {
	pipelineConfig := func(objects config.ObjectStorageConfig) *config.Config {
		GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
		cfg, err := config.NewLoader().Load(context.Background())
		Expect(err).NotTo(HaveOccurred())
		cfg.Attestation.Enabled = false
		cfg.Tenants.DefaultTenant = "crosscodexd-test"
		cfg.Storage.Objects = objects
		return cfg
	}
	fakeShared := func() *sharedResources {
		llm := &llmclienttest.Fake{}
		return &sharedResources{
			llmClient: llm, probeClient: llm, vectorDB: &vectordbtest.Fake{}, natsClient: &natsbustest.Fake{},
		}
	}

	It("builds with the s3 backend and no base_path, without contacting the endpoint", func() {
		cfg := pipelineConfig(config.ObjectStorageConfig{
			Backend: "s3", Bucket: "crosscodex-objects", Endpoint: "http://127.0.0.1:1",
		})

		svc, err := buildPipelineService(cfg, fakeShared())
		Expect(err).NotTo(HaveOccurred())
		Expect(svc).NotTo(BeNil())
	})

	DescribeTable("refuses an object storage setting it cannot start with",
		func(objects config.ObjectStorageConfig, want string) {
			svc, err := buildPipelineService(pipelineConfig(objects), fakeShared())
			Expect(svc).To(BeNil())
			Expect(err).To(MatchError(want))
		},
		Entry("the local backend without a base path",
			config.ObjectStorageConfig{Backend: "local"}, "pipeline: storage.objects.base_path must be set"),
		Entry("no backend",
			config.ObjectStorageConfig{BasePath: "/tmp/does-not-matter"},
			`pipeline: storage.objects.backend "" is not supported. Set it to "local" or "s3"`),
	)
})

// Catalog import resolves the caller's tenant from its X.509 client
// certificate, so a gateway-server listener that cannot verify client certs
// would accept the role and then fail every import as Unauthenticated. The
// refusal must happen at startup, before any database access: the mutual case
// uses an invalid tenant ID so the next guard (tenant.WithTenant) fails without
// touching the absent pool, proving the TLS check let it through. A mapping is
// set so the x509 mappings guard lets it through too.
var _ = Describe("buildIngestGateway client-certificate guard", func() {
	DescribeTable("requires a gateway-server listener that verifies client certificates",
		func(role, mode string, wantRefused bool) {
			cfg := &config.Config{}
			cfg.Tenants.DefaultTenant = "Not A Valid Tenant!"
			cfg.Auth.X509Mappings = []config.X509MappingConfig{
				{Match: config.X509MatchConfig{CN: "client"}, Roles: []string{authn.RoleAdmin}},
			}
			cfg.Storage.Objects = config.ObjectStorageConfig{Backend: "local", BasePath: GinkgoT().TempDir()}
			if mode != "off" {
				cfg.TLS = config.TLSConfig{
					Mode: mode,
					CA:   testTLSWiringCAPath,
					Cert: testTLSWiringClientCertPath,
					Key:  testTLSWiringClientKeyPath,
				}
			}

			svc, err := buildIngestGateway(context.Background(), role, cfg, &sharedResources{}, nil, nil)
			Expect(svc).To(BeNil())
			if wantRefused {
				Expect(err).To(MatchError(role + ` role: catalog import authenticates clients by X.509 certificate, so tls.mode (or tls.targets.gateway-server.mode) must be "mutual" with a CA configured`))
				return
			}
			Expect(err).To(MatchError(HavePrefix(role + " role: tenants.default_tenant: ")))
		},
		Entry("gateway role, tls off", config.RoleGateway, "off", true),
		Entry("gateway role, server-only tls", config.RoleGateway, "server-only", true),
		Entry("gateway role, mutual tls", config.RoleGateway, "mutual", false),
		Entry("all role, tls off", config.RoleAll, "off", true),
		Entry("all role, server-only tls", config.RoleAll, "server-only", true),
		Entry("all role, mutual tls", config.RoleAll, "mutual", false),
	)
})

// Roles come only from auth.x509_mappings, so a gateway-serving role with none
// would accept every TLS handshake and then refuse every caller. It refuses to
// start instead, before any database access, showing a mapping to add.
var _ = Describe("buildIngestGateway x509 mappings guard", func() {
	DescribeTable("refuses to start the gateway with no x509 mappings, showing one to add",
		func(role string) {
			cfg := &config.Config{}
			cfg.Tenants.DefaultTenant = "crosscodexd-test"
			cfg.Storage.Objects = config.ObjectStorageConfig{Backend: "local", BasePath: GinkgoT().TempDir()}
			cfg.TLS = config.TLSConfig{
				Mode: "mutual", CA: testTLSWiringCAPath, Cert: testTLSWiringClientCertPath, Key: testTLSWiringClientKeyPath,
			}

			svc, err := buildIngestGateway(context.Background(), role, cfg, &sharedResources{}, nil, nil)
			Expect(svc).To(BeNil())
			Expect(err).To(MatchError(errNoX509Mappings))
			Expect(err).To(MatchError(role + " role: no client certificate can sign in: auth.x509_mappings is empty, " +
				"and certificates no longer get admin implicitly. Add a mapping, e.g. for the certificates from deploy/certgen: " +
				"auth.x509_mappings: [{match: {cn: client, organization: CrossCodex}, roles: [admin]}]"))
		},
		Entry("gateway role", config.RoleGateway),
		Entry("all role", config.RoleAll),
	)
})

// allTenantAuthRegistry is the gateway's only source of identities: the first
// mapping a certificate matches decides its roles and, with tenants.enabled,
// its tenant; a certificate that matches none is refused.
var _ = Describe("allTenantAuthRegistry", func() {
	var ca *pkigen.CertKeyPair

	BeforeEach(func() {
		var err error
		ca, err = pkigen.GenerateCA(pkigen.WithOrganization("Registry Test"))
		Expect(err).NotTo(HaveOccurred())
	})

	authenticate := func(cfg *config.Config, cn, org string) (*authn.Identity, error) {
		leaf, err := pkigen.GenerateCert(ca, pkigen.WithOrganization(org), pkigen.WithDNSNames(cn),
			pkigen.WithExtKeyUsage(x509.ExtKeyUsageClientAuth))
		Expect(err).NotTo(HaveOccurred())
		registry, err := allTenantAuthRegistry(cfg)
		Expect(err).NotTo(HaveOccurred())
		return registry.Authenticate(context.Background(), &authn.Request{
			Method:   authn.AuthMethodMTLS,
			TLSState: &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf.Cert}},
		})
	}

	mappingsConfig := func(tenantsEnabled bool, tenantID string) *config.Config {
		cfg := &config.Config{}
		cfg.Tenants = config.TenantsConfig{Enabled: tenantsEnabled, DefaultTenant: "crosscodexd-test"}
		cfg.Auth.X509Mappings = []config.X509MappingConfig{
			{Match: config.X509MatchConfig{CN: "operator", Organization: "Acme"}, Tenant: tenantID, Roles: []string{authn.RoleAdmin}},
			{Match: config.X509MatchConfig{CN: "alice", Organization: "Acme"}, Tenant: tenantID, Roles: []string{authn.RoleReviewer}},
		}
		return cfg
	}

	DescribeTable("authenticates the mapped certificate with its mapped roles",
		func(tenantsEnabled bool, mappingTenant, cn string, wantRoles []string, wantTenant string) {
			identity, err := authenticate(mappingsConfig(tenantsEnabled, mappingTenant), cn, "Acme")
			Expect(err).NotTo(HaveOccurred())
			Expect(identity.Roles).To(Equal(wantRoles))
			Expect(identity.TenantID).To(Equal(wantTenant))
		},
		Entry("single-tenant admin mapping", false, "", "operator", []string{authn.RoleAdmin}, "crosscodexd-test"),
		Entry("single-tenant reviewer mapping", false, "", "alice", []string{authn.RoleReviewer}, "crosscodexd-test"),
		Entry("multi-tenant mapping, in the mapping's tenant", true, "acme", "operator", []string{authn.RoleAdmin}, "acme"),
	)

	DescribeTable("refuses a certificate no mapping matches",
		func(tenantsEnabled bool, mappingTenant, cn, org string) {
			identity, err := authenticate(mappingsConfig(tenantsEnabled, mappingTenant), cn, org)
			Expect(identity).To(BeNil())
			Expect(err).To(MatchError(authn.ErrAuthenticationFailed))
		},
		Entry("single-tenant, unmapped common name", false, "", "mallory", "Acme"),
		Entry("single-tenant, mapped common name in another organization", false, "", "operator", "Other"),
		Entry("multi-tenant, unmapped common name", true, "acme", "mallory", "Acme"),
	)

	It("refuses a mapping for another tenant when tenants.enabled is false", func() {
		registry, err := allTenantAuthRegistry(mappingsConfig(false, "acme"))
		Expect(registry).To(BeNil())
		Expect(err).To(MatchError(ContainSubstring("in single-tenant mode leave tenant empty or set it to the default tenant")))
	})
})

// buildPipelineService resolves attestation before anything else, so these
// specs need no shared resources: a refusal returns first, and a disabled
// attestation is proved to pass by reaching the next guard.
var _ = Describe("buildPipelineService attestation guards", func() {
	captureLogs := func() *bytes.Buffer {
		var buf bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
		DeferCleanup(func() { slog.SetDefault(prev) })
		return &buf
	}

	It("refuses to start when attestation is enabled without a key pair, naming the setting and both remedies", func() {
		cfg := &config.Config{}
		cfg.Attestation = config.AttestationConfig{Enabled: true, ExpiryDuration: time.Hour}

		svc, err := buildPipelineService(cfg, &sharedResources{})
		Expect(svc).To(BeNil())
		Expect(err).To(MatchError(config.ErrMissingRequired))
		Expect(err.Error()).To(And(
			ContainSubstring("attestation.private_key_path"), ContainSubstring("attestation.enabled: false")))
	})

	It("refuses to start when the configured key pair does not load, naming the file", func() {
		dir := GinkgoT().TempDir()
		missing := filepath.Join(dir, "attestation-key.pem")
		cfg := &config.Config{}
		cfg.Attestation = config.AttestationConfig{
			Enabled: true, PrivateKeyPath: missing, PublicKeyPath: filepath.Join(dir, "attestation-pub.pem"),
			ExpiryDuration: time.Hour,
		}

		svc, err := buildPipelineService(cfg, &sharedResources{})
		Expect(svc).To(BeNil())
		Expect(err).To(MatchError(attestation.ErrKeyNotFound))
		Expect(err.Error()).To(ContainSubstring(missing))
	})

	It("reads no key and logs once that attestation, and so result reuse, is disabled when it is", func() {
		logs := captureLogs()
		cfg := &config.Config{}
		cfg.Attestation = config.AttestationConfig{
			Enabled: false, PrivateKeyPath: "/nonexistent/key.pem", PublicKeyPath: "/nonexistent/pub.pem",
		}

		_, err := buildPipelineService(cfg, &sharedResources{})
		Expect(err).To(MatchError("pipeline: tenants.default_tenant must be set"))
		Expect(bytes.Count(logs.Bytes(), []byte("attestation is disabled"))).To(Equal(1))
		Expect(logs.String()).To(ContainSubstring("reuse no cached LLM results"))
	})

	It("logs each tenant whose override disables the attestation the global settings enable, and so its result reuse", func() {
		logs := captureLogs()
		dir := GinkgoT().TempDir()
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		Expect(err).NotTo(HaveOccurred())
		privDER, err := x509.MarshalECPrivateKey(key)
		Expect(err).NotTo(HaveOccurred())
		pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
		Expect(err).NotTo(HaveOccurred())
		priv, pub := filepath.Join(dir, "key.pem"), filepath.Join(dir, "pub.pem")
		Expect(os.WriteFile(priv, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privDER}), 0o600)).To(Succeed())
		Expect(os.WriteFile(pub, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), 0o600)).To(Succeed())
		no := false
		cfg := &config.Config{}
		cfg.Attestation = config.AttestationConfig{
			Enabled: true, PrivateKeyPath: priv, PublicKeyPath: pub, ExpiryDuration: time.Hour,
			TenantOverrides: map[string]config.AttestationOverride{"acme": {Enabled: &no}},
		}

		_, err = buildPipelineService(cfg, &sharedResources{})
		Expect(err).To(MatchError("pipeline: tenants.default_tenant must be set"))
		Expect(logs.String()).To(And(ContainSubstring("attestation is disabled for a tenant"), ContainSubstring("tenant_id=acme"),
			ContainSubstring("reuse no cached LLM results")))
		Expect(logs.String()).NotTo(ContainSubstring("attestation is disabled (attestation.enabled: false)"))
	})
})

// Any certificate the CA signed could name any tenant in x-tenant-id, so the
// pipeline role honors that header only from tls.service_identities. With none
// listed it would deny every request, so it refuses to start instead, before
// the database is touched (the zero sharedResources has no pool).
var _ = Describe("attachPipeline service identities guard", func() {
	pipelineConfig := func(identities []config.X509MatchConfig) *config.Config {
		cfg := &config.Config{}
		cfg.Pipeline.Addr = "127.0.0.1:0"
		cfg.TLS = config.TLSConfig{
			Mode:              "mutual",
			CA:                testTLSWiringCAPath,
			Cert:              testTLSWiringClientCertPath,
			Key:               testTLSWiringClientKeyPath,
			ServiceIdentities: identities,
		}
		return cfg
	}

	It("refuses to start with no service identities, naming the setting and a subject to add", func() {
		rt := &runtime{}
		err := attachPipeline(context.Background(), pipelineConfig(nil), &sharedResources{}, rt)
		Expect(err).To(MatchError(errNoServiceIdentities("pipeline", "gateway")))
		Expect(err).To(MatchError(ContainSubstring("tls.service_identities")))
		Expect(err).To(MatchError(ContainSubstring("cn: client")))
		Expect(rt.pipelineServer).To(BeNil(), "no listener may be built")
	})

	It("gets past the guard when a service identity is listed", func() {
		cfg := pipelineConfig([]config.X509MatchConfig{{CN: "client", Organization: "CrossCodex"}})
		err := attachPipeline(context.Background(), cfg, &sharedResources{}, &runtime{})
		Expect(err).To(MatchError(ContainSubstring("tenants.default_tenant must be set")))
	})
})

// unusedAddr returns a loopback address nothing listens on: it binds a free
// port and releases it, so a later dial fails unless something rebinds it.
func unusedAddr() string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred())
	addr := l.Addr().String()
	Expect(l.Close()).To(Succeed())
	return addr
}

// The graph role's listener serves the gateway's graph, crosswalk and cleaning
// RPCs, so it refuses to start on any missing safety precondition, before
// binding a listener or touching the database (the zero sharedResources has
// no pool).
var _ = Describe("attachGraphListener preconditions", func() {
	DescribeTable("refuses to start, naming the setting, and binds no listener",
		func(mutate func(*config.Config), wantKey string) {
			addr := unusedAddr()
			cfg := &config.Config{}
			cfg.Graph.Addr = addr
			cfg.Pipeline.Endpoint = unusedAddr()
			cfg.Tenants.DefaultTenant = "crosscodexd-test"
			cfg.TLS = config.TLSConfig{
				Mode:              "mutual",
				CA:                testTLSWiringCAPath,
				Cert:              testTLSWiringClientCertPath,
				Key:               testTLSWiringClientKeyPath,
				ServiceIdentities: []config.X509MatchConfig{{CN: "client", Organization: "CrossCodex"}},
			}
			mutate(cfg)
			rt := &runtime{graphService: &graph.Service{}}

			err := attachGraphListener(context.Background(), cfg, &sharedResources{}, rt)

			Expect(err).To(MatchError(And(HavePrefix("graph role: "), ContainSubstring(wantKey))))
			Expect(rt.graphServer).To(BeNil(), "no listener may be built")
			_, dialErr := net.DialTimeout("tcp", addr, time.Second)
			Expect(dialErr).To(HaveOccurred(), "nothing may listen on graph.addr")
		},
		Entry("no graph.addr", func(c *config.Config) { c.Graph.Addr = "" }, "graph.addr"),
		Entry("server-only TLS", func(c *config.Config) { c.TLS.Mode = "server-only" }, `"mutual"`),
		Entry("no tls.service_identities", func(c *config.Config) { c.TLS.ServiceIdentities = nil }, "tls.service_identities"),
		Entry("no tenants.default_tenant", func(c *config.Config) { c.Tenants.DefaultTenant = "" }, "tenants.default_tenant"),
		Entry("no pipeline.endpoint", func(c *config.Config) { c.Pipeline.Endpoint = "" }, "pipeline.endpoint"),
	)

	It("names the gateway's client certificate as the subject to list", func() {
		err := errNoServiceIdentities("graph", "gateway")
		Expect(err).To(MatchError(ContainSubstring("graph role: no client certificate may assert a tenant")))
		Expect(err).To(MatchError(ContainSubstring("List the gateway's client certificate subject")))
	})
})

// A standalone gateway serves graph, crosswalk and cleaning RPCs only through
// a graph role, so it refuses to start without one to reach.
var _ = Describe("attachGateway graph endpoint guard", func() {
	It("refuses to start without graph.endpoint, naming it, and builds no server", func() {
		cfg := &config.Config{}
		cfg.Pipeline.Endpoint = unusedAddr()
		rt := &runtime{}

		err := attachGateway(context.Background(), cfg, &sharedResources{}, rt)

		Expect(err).To(MatchError(ContainSubstring("graph.endpoint must be set")))
		Expect(rt.gatewayServer).To(BeNil())
	})
})

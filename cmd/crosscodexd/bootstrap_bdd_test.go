//go:build !integration

package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/graph"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/config"
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
		func(role, defaultTenant, basePath, want string) {
			cfg := &config.Config{}
			cfg.Tenants.DefaultTenant = defaultTenant
			cfg.Storage.Objects.BasePath = basePath

			svc, err := buildIngestGateway(context.Background(), role, cfg, &sharedResources{}, nil)
			Expect(svc).To(BeNil())
			Expect(err).To(MatchError(want))
		},
		Entry("gateway role without a default tenant",
			config.RoleGateway, "", "/tmp/does-not-matter", "gateway role: tenants.default_tenant must be set"),
		Entry("gateway role without a storage base path",
			config.RoleGateway, "crosscodexd-test", "", "gateway role: storage.objects.base_path must be set"),
		Entry("all role without a default tenant",
			config.RoleAll, "", "/tmp/does-not-matter", "all role: tenants.default_tenant must be set"),
		Entry("all role without a storage base path",
			config.RoleAll, "crosscodexd-test", "", "all role: storage.objects.base_path must be set"),
	)
})

// Catalog import resolves the caller's tenant from its X.509 client
// certificate, so a gateway-server listener that cannot verify client certs
// would accept the role and then fail every import as Unauthenticated. The
// refusal must happen at startup, before any database access: the mutual case
// uses an invalid tenant ID so the next guard (tenant.WithTenant) fails without
// touching the absent pool, proving the TLS check let it through.
var _ = Describe("buildIngestGateway client-certificate guard", func() {
	DescribeTable("requires a gateway-server listener that verifies client certificates",
		func(role, mode string, wantRefused bool) {
			cfg := &config.Config{}
			cfg.Tenants.DefaultTenant = "Not A Valid Tenant!"
			cfg.Storage.Objects.BasePath = GinkgoT().TempDir()
			if mode != "off" {
				cfg.TLS = config.TLSConfig{
					Mode: mode,
					CA:   testTLSWiringCAPath,
					Cert: testTLSWiringClientCertPath,
					Key:  testTLSWiringClientKeyPath,
				}
			}

			svc, err := buildIngestGateway(context.Background(), role, cfg, &sharedResources{}, nil)
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

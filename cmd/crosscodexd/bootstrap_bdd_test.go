//go:build !integration

package main

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/graph"
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

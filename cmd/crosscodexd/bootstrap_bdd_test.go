//go:build !integration

package main

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

// attachAllGateway's two config guards short-circuit before any shared
// resource is touched, so they are exercised directly here rather than through
// bootstrap(): for role=all, buildPipelineService validates the same two fields
// first and would mask these guards if reached via the full bootstrap path.
var _ = Describe("attachAllGateway config guards", func() {
	It("fails closed when no default tenant is configured", func() {
		cfg := &config.Config{}
		cfg.Tenants.DefaultTenant = ""
		cfg.Storage.Objects.BasePath = "/tmp/does-not-matter"

		err := attachAllGateway(context.Background(), cfg, &sharedResources{}, &runtime{}, nil)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("tenants.default_tenant must be set"))
	})

	It("fails closed when no storage base path is configured", func() {
		cfg := &config.Config{}
		cfg.Tenants.DefaultTenant = "crosscodexd-test"
		cfg.Storage.Objects.BasePath = ""

		err := attachAllGateway(context.Background(), cfg, &sharedResources{}, &runtime{}, nil)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("storage.objects.base_path must be set"))
	})
})

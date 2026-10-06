package config_test

import (
	"context"
	"errors"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

var _ = Describe("TenantsConfig.DefaultTenant", func() {
	It("defaults to empty, which roles without a default tenant accept", func() {
		cfg, err := config.NewLoader().Load(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Tenants.DefaultTenant).To(BeEmpty())
	})

	DescribeTable("accepts a valid tenant ID",
		func(value string) {
			GinkgoT().Setenv("CROSSCODEX_TENANTS_DEFAULT_TENANT", value)
			cfg, err := config.NewLoader().Load(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.Tenants.DefaultTenant).To(Equal(value))
		},
		Entry("the shipped deploy value", "default"),
		Entry("the shortest ID", "abc"),
		Entry("the longest ID, 52 characters", "a"+strings.Repeat("b", 51)),
	)

	DescribeTable("rejects an invalid tenant ID with an actionable message",
		func(value string) {
			GinkgoT().Setenv("CROSSCODEX_TENANTS_DEFAULT_TENANT", value)
			_, err := config.NewLoader().Load(context.Background())
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
			Expect(err.Error()).To(ContainSubstring("tenants.default_tenant %q", value))
			Expect(err.Error()).To(ContainSubstring("CROSSCODEX_TENANTS_DEFAULT_TENANT"))
			Expect(err.Error()).To(ContainSubstring(tenant.IDRule))
		},
		Entry("53 characters, one over the graph-name limit", "a"+strings.Repeat("b", 52)),
		Entry("uppercase", "Acme"),
		Entry("trailing hyphen", "acme-"),
		Entry("too short", "ab"),
	)
})

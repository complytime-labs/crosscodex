package config_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

var _ = Describe("ResolveRole", func() {
	DescribeTable("canonical roles resolve to themselves",
		func(role string) {
			resolved, err := config.ResolveRole(role)
			Expect(err).NotTo(HaveOccurred())
			Expect(resolved).To(Equal(role))
		},
		Entry("all", config.RoleAll),
		Entry("gateway", config.RoleGateway),
		Entry("pipeline", config.RolePipeline),
		Entry("worker", config.RoleWorker),
		Entry("graph", config.RoleGraph),
	)

	DescribeTable("deployment-facing aliases resolve to pipeline",
		func(alias string) {
			resolved, err := config.ResolveRole(alias)
			Expect(err).NotTo(HaveOccurred())
			Expect(resolved).To(Equal(config.RolePipeline))
		},
		Entry("analysis", "analysis"),
		Entry("synthesis", "synthesis"),
	)

	It("rejects an unknown role with an actionable error", func() {
		_, err := config.ResolveRole("nonexistent")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("nonexistent"))
		Expect(err.Error()).To(ContainSubstring("all, gateway, pipeline, worker, graph"))
		Expect(err).To(MatchError(config.ErrInvalidConfig))
	})

	It("rejects an empty role", func() {
		_, err := config.ResolveRole("")
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("Config.Role and Config.Health defaults", func() {
	It("defaults role to \"all\" and health.addr to \":9091\"", func() {
		cfg, err := config.NewLoader().Load(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Role).To(Equal(config.RoleAll))
		Expect(cfg.Health.Addr).To(Equal(":9091"))
	})

	It("normalizes an alias supplied via CROSSCODEX_ROLE to its canonical role", func() {
		GinkgoT().Setenv("CROSSCODEX_ROLE", "analysis")
		cfg, err := config.NewLoader().Load(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Role).To(Equal(config.RolePipeline))
	})

	It("rejects an invalid role from CROSSCODEX_ROLE with an actionable, chain-matchable error", func() {
		GinkgoT().Setenv("CROSSCODEX_ROLE", "nonexistent")
		_, err := config.NewLoader().Load(context.Background())
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("nonexistent"))
		Expect(err).To(MatchError(config.ErrInvalidConfig),
			"validateRole must wrap with %w, not %s, so errors.Is(err, ErrInvalidConfig) still matches")
	})

	DescribeTable("every canonical role and alias loads without error",
		func(role string) {
			GinkgoT().Setenv("CROSSCODEX_ROLE", role)
			_, err := config.NewLoader().Load(context.Background())
			Expect(err).NotTo(HaveOccurred())
		},
		Entry("all", config.RoleAll),
		Entry("gateway", config.RoleGateway),
		Entry("pipeline", config.RolePipeline),
		Entry("worker", config.RoleWorker),
		Entry("graph", config.RoleGraph),
		Entry("analysis alias", "analysis"),
		Entry("synthesis alias", "synthesis"),
	)
})

// Loading configuration must not enforce the split-role NATS requirement:
// the crosscodex CLI, crosscodexd healthcheck and the admin subcommands all
// load the same config and never start a role. crosscodexd enforces it once,
// on the effective role, after its --role flag is applied.
var _ = Describe("Config.Role and nats.url", func() {
	It("defaults nats.url to empty (embedded) for the default role all", func() {
		cfg, err := config.NewLoader().Load(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Role).To(Equal(config.RoleAll))
		Expect(cfg.NATS.URL).To(BeEmpty())
	})

	DescribeTable("loads a split role with an empty nats.url",
		func(role string) {
			GinkgoT().Setenv("CROSSCODEX_ROLE", role)
			cfg, err := config.NewLoader().Load(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.Role).To(Equal(role))
			Expect(cfg.NATS.URL).To(BeEmpty())
		},
		Entry("gateway", config.RoleGateway),
		Entry("pipeline", config.RolePipeline),
		Entry("worker", config.RoleWorker),
		Entry("graph", config.RoleGraph),
	)

	It("loads a split role with nats.url set", func() {
		GinkgoT().Setenv("CROSSCODEX_ROLE", config.RoleWorker)
		GinkgoT().Setenv("CROSSCODEX_NATS_URL", "nats://nats.example.com:4222")
		cfg, err := config.NewLoader().Load(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.NATS.URL).To(Equal("nats://nats.example.com:4222"))
	})
})

var _ = Describe("RequireSharedNATS", func() {
	DescribeTable("rejects every split role with an empty URL",
		func(role string) {
			err := config.RequireSharedNATS(role, "")
			Expect(err).To(MatchError(config.ErrInvalidConfig))
			Expect(err.Error()).To(ContainSubstring(`role "` + role + `" requires nats.url`))
			Expect(err.Error()).To(ContainSubstring("CROSSCODEX_NATS_URL"))
			Expect(err.Error()).To(ContainSubstring("set the role to all (--role all or CROSSCODEX_ROLE=all)"))
		},
		Entry("gateway", config.RoleGateway),
		Entry("pipeline", config.RolePipeline),
		Entry("worker", config.RoleWorker),
		Entry("graph", config.RoleGraph),
	)

	DescribeTable("accepts",
		func(role, url string) {
			Expect(config.RequireSharedNATS(role, url)).To(Succeed())
		},
		Entry("role all with an empty URL", config.RoleAll, ""),
		Entry("role all with a URL", config.RoleAll, "nats://nats:4222"),
		Entry("a split role with a URL", config.RoleWorker, "nats://nats:4222"),
	)
})

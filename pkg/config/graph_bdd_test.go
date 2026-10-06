package config_test

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

var _ = Describe("GraphConfig.MaxBulkEdges", func() {
	It("defaults to DefaultGraphMaxBulkEdges", func() {
		cfg, err := config.NewLoader().Load(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Graph.MaxBulkEdges).To(Equal(config.DefaultGraphMaxBulkEdges))
		Expect(config.DefaultGraphMaxBulkEdges).To(Equal(1000))
	})

	DescribeTable("accepts a valid override",
		func(value string, want int) {
			GinkgoT().Setenv("CROSSCODEX_GRAPH_MAX_BULK_EDGES", value)
			cfg, err := config.NewLoader().Load(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.Graph.MaxBulkEdges).To(Equal(want))
		},
		Entry("lower bound", "1", 1),
		Entry("typical", "250", 250),
		Entry("upper bound", "10000", 10000),
	)

	DescribeTable("rejects an out-of-range value with an actionable message",
		func(value string) {
			GinkgoT().Setenv("CROSSCODEX_GRAPH_MAX_BULK_EDGES", value)
			_, err := config.NewLoader().Load(context.Background())
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, config.ErrInvalidConfig)).To(BeTrue())
			Expect(err.Error()).To(ContainSubstring("graph.max_bulk_edges " + value))
			Expect(err.Error()).To(ContainSubstring("CROSSCODEX_GRAPH_MAX_BULK_EDGES"))
			Expect(err.Error()).To(ContainSubstring("must be in range [1, 10000]"))
		},
		Entry("zero", "0"),
		Entry("negative", "-5"),
		Entry("above the upper bound", "10001"),
	)
})

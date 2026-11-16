package graph_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/graph"
	"github.com/complytime-labs/crosscodex/pkg/config"
)

var _ = Describe("WithMaxSimilarityLimit", func() {
	newService := func(opts ...graph.Option) *graph.Service {
		return graph.New(&mockGraphDB{}, &mockVectorDB{}, nil, opts...)
	}

	It("applies config.DefaultGraphMaxSimilarityLimit when no limit is configured", func() {
		Expect(graph.ExportMaxSimilarityLimit(newService())).To(Equal(config.DefaultGraphMaxSimilarityLimit))
	})

	It("applies a configured limit", func() {
		Expect(graph.ExportMaxSimilarityLimit(newService(graph.WithMaxSimilarityLimit(5)))).To(Equal(5))
	})

	DescribeTable("ignores a non-positive limit and keeps config.DefaultGraphMaxSimilarityLimit",
		func(n int) {
			Expect(graph.ExportMaxSimilarityLimit(newService(graph.WithMaxSimilarityLimit(n)))).To(Equal(config.DefaultGraphMaxSimilarityLimit))
		},
		Entry("zero", 0),
		Entry("negative", -3),
	)
})

var _ = Describe("WithMaxSimilarityCatalogs", func() {
	newService := func(opts ...graph.Option) *graph.Service {
		return graph.New(&mockGraphDB{}, &mockVectorDB{}, nil, opts...)
	}

	It("applies config.DefaultGraphMaxSimilarityCatalogs when no limit is configured", func() {
		Expect(graph.ExportMaxSimilarityCatalogs(newService())).To(Equal(config.DefaultGraphMaxSimilarityCatalogs))
	})

	It("applies a configured limit", func() {
		Expect(graph.ExportMaxSimilarityCatalogs(newService(graph.WithMaxSimilarityCatalogs(5)))).To(Equal(5))
	})

	DescribeTable("ignores a non-positive limit and keeps config.DefaultGraphMaxSimilarityCatalogs",
		func(n int) {
			Expect(graph.ExportMaxSimilarityCatalogs(newService(graph.WithMaxSimilarityCatalogs(n)))).To(Equal(config.DefaultGraphMaxSimilarityCatalogs))
		},
		Entry("zero", 0),
		Entry("negative", -3),
	)
})

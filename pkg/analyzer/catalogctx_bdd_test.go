package analyzer_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/analyzer"
)

var _ = Describe("Catalog ID Context", func() {
	It("round-trips a catalog ID through the context", func() {
		ctx := analyzer.WithCatalogID(context.Background(), "cat-1")
		got, ok := analyzer.CatalogIDFromContext(ctx)
		Expect(ok).To(BeTrue())
		Expect(got).To(Equal("cat-1"))
	})

	It("reports absent when no catalog ID is present", func() {
		_, ok := analyzer.CatalogIDFromContext(context.Background())
		Expect(ok).To(BeFalse())
	})
})

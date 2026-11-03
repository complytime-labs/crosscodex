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

	It("returns each control's own catalog", func() {
		ctx := analyzer.WithControlCatalogs(context.Background(), map[string]string{"cat-a/AC-1": "cat-a", "cat-b/PR.AA-01": "cat-b"})
		got, ok := analyzer.ControlCatalogFromContext(ctx, "cat-b/PR.AA-01")
		Expect(ok).To(BeTrue())
		Expect(got).To(Equal("cat-b"))
	})

	DescribeTable("reports absent when no catalog is recorded for the control",
		func(ctx context.Context) {
			_, ok := analyzer.ControlCatalogFromContext(ctx, "cat-a/AC-1")
			Expect(ok).To(BeFalse())
		},
		Entry("no map in the context", context.Background()),
		Entry("the control is not in the map", analyzer.WithControlCatalogs(context.Background(), map[string]string{"cat-a/AC-2": "cat-a"})),
		Entry("the control's catalog is empty", analyzer.WithControlCatalogs(context.Background(), map[string]string{"cat-a/AC-1": ""})),
	)

	DescribeTable("reports whether a per-control catalog map was recorded",
		func(ctx context.Context, want bool) {
			Expect(analyzer.HasControlCatalogs(ctx)).To(Equal(want))
		},
		Entry("no map in the context", context.Background(), false),
		Entry("only the run's catalog", analyzer.WithCatalogID(context.Background(), "cat-a"), false),
		Entry("an empty map", analyzer.WithControlCatalogs(context.Background(), map[string]string{}), true),
		Entry("a populated map", analyzer.WithControlCatalogs(context.Background(), map[string]string{"cat-a/AC-1": "cat-a"}), true),
	)
})

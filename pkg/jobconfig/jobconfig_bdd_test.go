package jobconfig_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/jobconfig"
)

var _ = Describe("ExtractCatalogs", func() {
	DescribeTable("reads the job's catalogs and direction",
		func(stored string, want jobconfig.Catalogs, wantIDs []string) {
			got, ok := jobconfig.ExtractCatalogs([]byte(stored))
			Expect(ok).To(BeTrue())
			Expect(got).To(Equal(want))
			Expect(got.CatalogIDs()).To(Equal(wantIDs))
		},
		Entry("a config from before cross-catalog analysis runs within its catalog",
			`{"catalog_id":"cat-a"}`,
			jobconfig.Catalogs{SourceID: "cat-a", TargetID: "cat-a"}, []string{"cat-a"}),
		Entry("target equal to source runs within one catalog",
			`{"catalog_id":"cat-a","target_catalog_id":"cat-a"}`,
			jobconfig.Catalogs{SourceID: "cat-a", TargetID: "cat-a"}, []string{"cat-a"}),
		Entry("a different target maps source onto target",
			`{"catalog_id":"cat-a","target_catalog_id":"cat-b"}`,
			jobconfig.Catalogs{SourceID: "cat-a", TargetID: "cat-b"}, []string{"cat-a", "cat-b"}),
		Entry("bidirectional with a different target pairs both ways",
			`{"catalog_id":"cat-a","target_catalog_id":"cat-b","bidirectional":true}`,
			jobconfig.Catalogs{SourceID: "cat-a", TargetID: "cat-b", Bidirectional: true}, []string{"cat-a", "cat-b"}),
		Entry("bidirectional within one catalog is dropped: that pass pairs both ways already",
			`{"catalog_id":"cat-a","target_catalog_id":"cat-a","bidirectional":true}`,
			jobconfig.Catalogs{SourceID: "cat-a", TargetID: "cat-a"}, []string{"cat-a"}),
		Entry("bidirectional without a target is dropped with it",
			`{"catalog_id":"cat-a","bidirectional":true}`,
			jobconfig.Catalogs{SourceID: "cat-a", TargetID: "cat-a"}, []string{"cat-a"}),
	)

	DescribeTable("refuses malformed configs and configs without a catalog_id",
		func(stored string) {
			got, ok := jobconfig.ExtractCatalogs([]byte(stored))
			Expect(ok).To(BeFalse())
			Expect(got).To(BeZero())
		},
		Entry("empty object", `{}`),
		Entry("empty catalog id", `{"catalog_id":""}`),
		Entry("non-string catalog id", `{"catalog_id":7}`),
		Entry("non-string target catalog id", `{"catalog_id":"cat-a","target_catalog_id":7}`),
		Entry("non-boolean bidirectional", `{"catalog_id":"cat-a","bidirectional":"yes"}`),
		Entry("pre-import-only oneof shape", `{"Source":{"CatalogId":"cat-1"}}`),
		Entry("malformed json", `{not json`),
		Entry("empty input", ``),
	)

	It("reports a crosswalk only when the target differs from the source", func() {
		Expect(jobconfig.Catalogs{SourceID: "a", TargetID: "a"}.CrossCatalog()).To(BeFalse())
		Expect(jobconfig.Catalogs{SourceID: "a", TargetID: "b"}.CrossCatalog()).To(BeTrue())
	})
})

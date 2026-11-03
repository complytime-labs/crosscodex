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

	Describe("the catalog labels recorded at creation", func() {
		csf := jobconfig.Label{Name: "CSF 2.0", Version: "2.0", Recorded: true}
		sp := jobconfig.Label{Name: "NIST SP 800-53", Recorded: true}

		DescribeTable("reads each side's snapshot",
			func(stored string, wantSource, wantTarget jobconfig.Label) {
				got, ok := jobconfig.ExtractCatalogs([]byte(stored))
				Expect(ok).To(BeTrue())
				Expect(got.SourceLabel).To(Equal(wantSource))
				Expect(got.TargetLabel).To(Equal(wantTarget))
			},
			Entry("a crosswalk records both sides",
				`{"catalog_id":"cat-a","target_catalog_id":"cat-b",`+
					`"source_catalog":{"catalog_id":"cat-a","name":"CSF 2.0","version":"2.0"},`+
					`"target_catalog":{"catalog_id":"cat-b","name":"NIST SP 800-53"}}`,
				csf, sp),
			Entry("within one catalog the target is the source's label",
				`{"catalog_id":"cat-a","target_catalog_id":"cat-a","source_catalog":{"catalog_id":"cat-a","name":"CSF 2.0","version":"2.0"}}`,
				csf, csf),
			Entry("a config from before snapshots records none",
				`{"catalog_id":"cat-a","target_catalog_id":"cat-b"}`,
				jobconfig.Label{}, jobconfig.Label{}),
			// Only CreateJob writes snapshots; one naming another catalog
			// was not written by it and labels nothing.
			Entry("a snapshot of another catalog is ignored",
				`{"catalog_id":"cat-a","target_catalog_id":"cat-b",`+
					`"source_catalog":{"catalog_id":"cat-b","name":"CSF 2.0"},"target_catalog":{"catalog_id":"cat-x","name":"NIST SP 800-53"}}`,
				jobconfig.Label{}, jobconfig.Label{}),
		)

		It("refuses a snapshot that is not an object", func() {
			_, ok := jobconfig.ExtractCatalogs([]byte(`{"catalog_id":"cat-a","source_catalog":"CSF 2.0"}`))
			Expect(ok).To(BeFalse())
		})

		It("looks a label up by catalog ID", func() {
			got, ok := jobconfig.ExtractCatalogs([]byte(`{"catalog_id":"cat-a","target_catalog_id":"cat-b",` +
				`"source_catalog":{"catalog_id":"cat-a","name":"CSF 2.0","version":"2.0"}}`))
			Expect(ok).To(BeTrue())
			Expect(got.LabelOf("cat-a")).To(Equal(csf))
			Expect(got.LabelOf("cat-b")).To(Equal(jobconfig.Label{}))
			Expect(got.LabelOf("cat-other")).To(Equal(jobconfig.Label{}))
		})
	})

	It("reports a crosswalk only when the target differs from the source", func() {
		Expect(jobconfig.Catalogs{SourceID: "a", TargetID: "a"}.CrossCatalog()).To(BeFalse())
		Expect(jobconfig.Catalogs{SourceID: "a", TargetID: "b"}.CrossCatalog()).To(BeTrue())
	})
})

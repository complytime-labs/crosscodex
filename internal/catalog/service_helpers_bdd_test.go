package catalog

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	crosscodexv1 "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
)

var _ = Describe("formatStringToEnum", func() {
	DescribeTable("maps format strings to the CatalogFormat enum",
		func(format string, want crosscodexv1.CatalogFormat) {
			Expect(formatStringToEnum(format)).To(Equal(want))
		},
		Entry("oscal lowercase", "oscal", crosscodexv1.CatalogFormat_CATALOG_FORMAT_OSCAL),
		Entry("oscal uppercase", "OSCAL", crosscodexv1.CatalogFormat_CATALOG_FORMAT_OSCAL),
		Entry("oscal mixed case", "OsCaL", crosscodexv1.CatalogFormat_CATALOG_FORMAT_OSCAL),
		Entry("gemara lowercase", "gemara", crosscodexv1.CatalogFormat_CATALOG_FORMAT_GEMARA),
		Entry("gemara uppercase", "GEMARA", crosscodexv1.CatalogFormat_CATALOG_FORMAT_GEMARA),
		Entry("unknown format", "unknown", crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED),
		Entry("empty string", "", crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED),
		Entry("oscal with leading whitespace", " oscal", crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED),
		Entry("oscal with trailing whitespace", "oscal ", crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED),
		Entry("oscal with surrounding whitespace", " oscal ", crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED),
		Entry("gemara with tabs", "\tgemara\t", crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED),
		Entry("oscal with newline", "oscal\n", crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED),
		Entry("partial oscal", "osc", crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED),
		Entry("partial gemara", "gem", crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED),
		Entry("oscal with suffix", "oscal-json", crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED),
		Entry("oscal with prefix", "json-oscal", crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED),
		Entry("typo osacl", "osacl", crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED),
		Entry("typo gemera", "gemera", crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED),
		Entry("numeric string", "123", crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED),
		Entry("special characters", "!@#$%", crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED),
	)
})

var _ = Describe("catalogRecordToProto", func() {
	Describe("Format field", func() {
		DescribeTable("maps the record's format string to the proto enum",
			func(formatString string, wantEnum crosscodexv1.CatalogFormat) {
				rec := &CatalogRecord{
					CatalogID: "test-catalog-id",
					TenantID:  "test-tenant",
					Name:      "Test Catalog",
					Format:    formatString,
				}

				proto := catalogRecordToProto(rec)

				Expect(proto).NotTo(BeNil())
				Expect(proto.GetFormat()).To(Equal(wantEnum))
				Expect(proto.GetName()).To(Equal("Test Catalog"))
			},
			Entry("OSCAL format", "oscal", crosscodexv1.CatalogFormat_CATALOG_FORMAT_OSCAL),
			Entry("Gemara format", "gemara", crosscodexv1.CatalogFormat_CATALOG_FORMAT_GEMARA),
			Entry("unspecified format", "", crosscodexv1.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED),
		)
	})

	It("maps the document lineage fields and leaves current to the service", func() {
		proto := catalogRecordToProto(&CatalogRecord{
			CatalogID: "c2", DocumentSlug: "nist-800-53", VersionLabel: "rev5",
			SupersedesCatalogID: "c1", LinkMethod: "declared",
		})

		Expect(proto.GetDocumentSlug()).To(Equal("nist-800-53"))
		Expect(proto.GetVersionLabel()).To(Equal("rev5"))
		Expect(proto.GetSupersedesCatalogId()).To(Equal("c1"))
		Expect(proto.GetLinkMethod()).To(Equal("declared"))
		Expect(proto.GetCurrent()).To(BeFalse())
	})

	Describe("nil record", func() {
		It("returns nil", func() {
			Expect(catalogRecordToProto(nil)).To(BeNil())
		})
	})
})

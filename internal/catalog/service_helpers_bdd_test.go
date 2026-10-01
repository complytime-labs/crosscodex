package catalog

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	crosscodexv1 "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
)

// generateLongString creates a string of repeated characters for boundary testing
func generateLongString(length int) string {
	return strings.Repeat("A", length)
}

var _ = Describe("extractCatalogName", func() {
	DescribeTable("extracts the catalog name from OSCAL JSON",
		func(oscalJSON string, want string) {
			Expect(extractCatalogName([]byte(oscalJSON))).To(Equal(want))
		},
		Entry("valid OSCAL with metadata title", `{
				"catalog": {
					"metadata": {
						"title": "CrossCodex E2E Minimal Catalog",
						"version": "1.0"
					}
				}
			}`, "CrossCodex E2E Minimal Catalog"),
		Entry("OSCAL without metadata", `{
				"catalog": {
					"groups": []
				}
			}`, ""),
		Entry("OSCAL without title", `{
				"catalog": {
					"metadata": {
						"version": "1.0"
					}
				}
			}`, ""),
		Entry("not OSCAL", `{
				"some_other_root": {}
			}`, ""),
		Entry("invalid JSON", `not json`, ""),
		Entry("empty", ``, ""),
		Entry("title with unicode characters", `{
				"catalog": {
					"metadata": {
						"title": "NIST SP 800-53 Rev. 5 — Security & Privacy Controls 🔒"
					}
				}
			}`, "NIST SP 800-53 Rev. 5 — Security & Privacy Controls 🔒"),
		Entry("title with escaped quotes", `{
				"catalog": {
					"metadata": {
						"title": "The \"Advanced\" Catalog"
					}
				}
			}`, `The "Advanced" Catalog`),
		Entry("title with newlines and tabs", `{
				"catalog": {
					"metadata": {
						"title": "Multi\nLine\tTitle"
					}
				}
			}`, "Multi\nLine\tTitle"),
		Entry("title with backslashes", `{
				"catalog": {
					"metadata": {
						"title": "Path\\To\\Catalog"
					}
				}
			}`, `Path\To\Catalog`),
		Entry("title is null", `{
				"catalog": {
					"metadata": {
						"title": null
					}
				}
			}`, ""),
		Entry("title is number", `{
				"catalog": {
					"metadata": {
						"title": 12345
					}
				}
			}`, ""),
		Entry("title is boolean", `{
				"catalog": {
					"metadata": {
						"title": true
					}
				}
			}`, ""),
		Entry("title is array", `{
				"catalog": {
					"metadata": {
						"title": ["Catalog", "Name"]
					}
				}
			}`, ""),
		Entry("title is object", `{
				"catalog": {
					"metadata": {
						"title": {"en": "English Title"}
					}
				}
			}`, ""),
		Entry("very long title", `{
				"catalog": {
					"metadata": {
						"title": "`+generateLongString(5000)+`"
					}
				}
			}`, generateLongString(5000)),
		Entry("empty string title", `{
				"catalog": {
					"metadata": {
						"title": ""
					}
				}
			}`, ""),
		Entry("metadata is null", `{
				"catalog": {
					"metadata": null
				}
			}`, ""),
		Entry("catalog is null", `{
				"catalog": null
			}`, ""),
	)
})

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

	Describe("nil record", func() {
		It("returns nil", func() {
			Expect(catalogRecordToProto(nil)).To(BeNil())
		})
	})
})

var _ = Describe("isOSCALJSON", func() {
	DescribeTable("detects OSCAL documents",
		func(data string, want bool) {
			Expect(isOSCALJSON([]byte(data))).To(Equal(want))
		},
		Entry("valid OSCAL JSON", `{"catalog": {"metadata": {"title": "Test"}}}`, true),
		Entry("OSCAL with empty catalog", `{"catalog": {}}`, true),
		Entry("catalog is null", `{"catalog": null}`, true),
		Entry("not OSCAL - missing catalog key", `{"metadata": {"title": "Test"}}`, false),
		Entry("invalid JSON", `not valid json`, false),
		Entry("empty string", ``, false),
		Entry("JSON array", `[{"catalog": {}}]`, false),
		Entry("JSON string", `"catalog"`, false),
		Entry("JSON number", `123`, false),
		Entry("JSON boolean", `true`, false),
		Entry("JSON null", `null`, false),
		Entry("catalog with wrong case", `{"Catalog": {}}`, false),
		Entry("catalog with extra whitespace", `{  "catalog"  :  {}  }`, true),
	)
})

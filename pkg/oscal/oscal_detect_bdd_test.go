package oscal_test

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/oscal"
)

var _ = Describe("DetectCatalogJSON", func() {
	DescribeTable("detects OSCAL catalog JSON",
		func(data string, want bool) {
			Expect(oscal.DetectCatalogJSON([]byte(data)).IsCatalog).To(Equal(want))
		},
		Entry("valid OSCAL JSON", `{"catalog": {"metadata": {"title": "Test"}}}`, true),
		Entry("OSCAL with empty catalog", `{"catalog": {}}`, true),
		Entry("catalog is null", `{"catalog": null}`, true),
		Entry("catalog with extra whitespace", `{  "catalog"  :  {}  }`, true),
		Entry("not OSCAL - missing catalog key", `{"metadata": {"title": "Test"}}`, false),
		Entry("an OSCAL profile", `{"profile": {"imports": []}}`, false),
		Entry("invalid JSON", `not valid json`, false),
		Entry("empty input", ``, false),
		Entry("JSON array", `[{"catalog": {}}]`, false),
		Entry("JSON string", `"catalog"`, false),
		Entry("JSON number", `123`, false),
		Entry("JSON boolean", `true`, false),
		Entry("JSON null", `null`, false),
		Entry("catalog with wrong case", `{"Catalog": {}}`, false),
		Entry("PDF magic bytes", "%PDF-1.7\n%\xe2\xe3\xcf\xd3\n", false),
	)

	DescribeTable("reads the title from OSCAL JSON metadata",
		func(data string, want string) {
			Expect(oscal.DetectCatalogJSON([]byte(data)).MetadataString("title")).To(Equal(want))
		},
		Entry("valid OSCAL with metadata title", `{"catalog": {"metadata": {"title": "CrossCodex E2E Minimal Catalog", "version": "1.0"}}}`, "CrossCodex E2E Minimal Catalog"),
		Entry("OSCAL without metadata", `{"catalog": {"groups": []}}`, ""),
		Entry("OSCAL without title", `{"catalog": {"metadata": {"version": "1.0"}}}`, ""),
		Entry("not OSCAL", `{"some_other_root": {"metadata": {"title": "x"}}}`, ""),
		Entry("invalid JSON", `not json`, ""),
		Entry("empty input", ``, ""),
		Entry("title with unicode characters", `{"catalog": {"metadata": {"title": "NIST SP 800-53 Rev. 5 — Security & Privacy Controls 🔒"}}}`, "NIST SP 800-53 Rev. 5 — Security & Privacy Controls 🔒"),
		Entry("title with escaped quotes", `{"catalog": {"metadata": {"title": "The \"Advanced\" Catalog"}}}`, `The "Advanced" Catalog`),
		Entry("title with newlines and tabs", `{"catalog": {"metadata": {"title": "Multi\nLine\tTitle"}}}`, "Multi\nLine\tTitle"),
		Entry("title with backslashes", `{"catalog": {"metadata": {"title": "Path\\To\\Catalog"}}}`, `Path\To\Catalog`),
		Entry("title is null", `{"catalog": {"metadata": {"title": null}}}`, ""),
		Entry("title is number", `{"catalog": {"metadata": {"title": 12345}}}`, ""),
		Entry("title is boolean", `{"catalog": {"metadata": {"title": true}}}`, ""),
		Entry("title is array", `{"catalog": {"metadata": {"title": ["Catalog", "Name"]}}}`, ""),
		Entry("title is object", `{"catalog": {"metadata": {"title": {"en": "English Title"}}}}`, ""),
		Entry("very long title", `{"catalog": {"metadata": {"title": "`+strings.Repeat("A", 5000)+`"}}}`, strings.Repeat("A", 5000)),
		Entry("empty string title", `{"catalog": {"metadata": {"title": ""}}}`, ""),
		Entry("metadata is null", `{"catalog": {"metadata": null}}`, ""),
		Entry("metadata is not an object", `{"catalog": {"metadata": "title"}}`, ""),
		Entry("catalog is null", `{"catalog": null}`, ""),
	)

	DescribeTable("IsModelJSON detects a JSON object rooted at any OSCAL model",
		func(data string, want bool) {
			Expect(oscal.IsModelJSON([]byte(data))).To(Equal(want))
		},
		Entry("a catalog", `{"catalog": {}}`, true),
		Entry("a profile", `{"profile": {"imports": []}}`, true),
		Entry("a component-definition", `{"component-definition": {"uuid": "x"}}`, true),
		Entry("a system-security-plan", `{"system-security-plan": {}}`, true),
		Entry("an assessment-plan", `{"assessment-plan": {}}`, true),
		Entry("assessment-results", `{"assessment-results": {}}`, true),
		Entry("a plan-of-action-and-milestones", `{"plan-of-action-and-milestones": {}}`, true),
		Entry("a mapping-collection", `{"mapping-collection": {"uuid": "x"}}`, true),
		Entry("a model key with a null value", `{"profile": null}`, true),
		Entry("a non-OSCAL JSON object", `{"schema_version": "1.0", "elements": []}`, false),
		Entry("a model key with the wrong case", `{"Profile": {}}`, false),
		Entry("a model key nested below the root", `{"wrapper": {"profile": {}}}`, false),
		Entry("a JSON array of a model", `[{"profile": {}}]`, false),
		Entry("a JSON string naming a model", `"profile"`, false),
		Entry("invalid JSON", `{"profile": `, false),
		Entry("empty input", ``, false),
		Entry("PDF magic bytes", "%PDF-1.7\n%\xe2\xe3\xcf\xd3\n", false),
	)

	It("lists the catalog and profile among the model root keys, and returns a copy", func() {
		keys := oscal.ModelRootKeys()
		Expect(keys).To(ContainElements(oscal.CatalogRootKey, oscal.ProfileRootKey, "mapping-collection"))
		keys[0] = "tampered"
		Expect(oscal.ModelRootKeys()).NotTo(ContainElement("tampered"))
	})

	DescribeTable("reads the version from OSCAL JSON metadata",
		func(data string, want string) {
			Expect(oscal.DetectCatalogJSON([]byte(data)).MetadataString("version")).To(Equal(want))
		},
		Entry("a version", `{"catalog": {"metadata": {"title": "SP 800-53", "version": "5.1.1"}}}`, "5.1.1"),
		Entry("no version", `{"catalog": {"metadata": {"title": "SP 800-53"}}}`, ""),
		Entry("a non-string version", `{"catalog": {"metadata": {"version": 5}}}`, ""),
		Entry("no metadata", `{"catalog": {}}`, ""),
		Entry("not OSCAL", `{"some_other_root": {}}`, ""),
		Entry("invalid JSON", `not json`, ""),
	)
})

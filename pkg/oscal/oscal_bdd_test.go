package oscal_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/oscal"
)

func TestOscalBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "OSCAL BDD Suite")
}

var _ = BeforeSuite(func() { DeferCleanup(testspecs.RedirectLogsToGinkgo()) })

var _ = Describe("Foundation Types", func() {
	Describe("Class Constants", func() {
		It("defines ClassSection correctly", func() {
			Expect(oscal.ClassSection).To(Equal("compliance-section"))
		})

		It("defines ClassRequirement correctly", func() {
			Expect(oscal.ClassRequirement).To(Equal("compliance-requirement"))
		})

		It("defines ClassContext correctly", func() {
			Expect(oscal.ClassContext).To(Equal("compliance-context"))
		})
	})

	Describe("ControlItem", func() {
		It("can be constructed with all fields", func() {
			item := oscal.ControlItem{
				ID:       "ac-2",
				Title:    "Account Management",
				Text:     "The organization manages information system accounts.",
				Class:    oscal.ClassRequirement,
				ParentID: "ac",
				GroupID:  "access-control",
				Props: map[string]string{
					"parent-id":   "ac",
					"source-part": "statement",
				},
			}

			Expect(item.ID).To(Equal("ac-2"))
			Expect(item.Title).To(Equal("Account Management"))
			Expect(item.Text).To(Equal("The organization manages information system accounts."))
			Expect(item.Class).To(Equal(oscal.ClassRequirement))
			Expect(item.ParentID).To(Equal("ac"))
			Expect(item.GroupID).To(Equal("access-control"))
			Expect(item.Props).To(HaveKeyWithValue("parent-id", "ac"))
			Expect(item.Props).To(HaveKeyWithValue("source-part", "statement"))
		})
	})
})

// findItem searches a slice of ControlItems for an item with the given ID.
func findItem(items []oscal.ControlItem, id string) *oscal.ControlItem {
	for i := range items {
		if items[i].ID == id {
			return &items[i]
		}
	}
	return nil
}

var _ = Describe("Parser", func() {
	var (
		parser oscal.Parser
		items  []oscal.ControlItem
	)

	// Shared setup: parse the minimal catalog fixture once for tests that need it.
	parseMinimalCatalog := func() {
		data, err := os.ReadFile("testdata/minimal_catalog.json")
		Expect(err).NotTo(HaveOccurred(), "failed to read test fixture")

		parser = oscal.NewParser("")
		items, err = parser.Parse(context.Background(), bytes.NewReader(data))
		Expect(err).NotTo(HaveOccurred(), "Parse() failed")
	}

	Context("parsing a minimal catalog", func() {
		BeforeEach(func() { parseMinimalCatalog() })

		It("produces at least 3 control items", func() {
			Expect(len(items)).To(BeNumerically(">=", 3))
		})
	})

	Context("AC-2 decomposition", func() {
		BeforeEach(func() { parseMinimalCatalog() })

		It("emits AC-2 parent as ClassSection", func() {
			ac2 := findItem(items, "ac-2")
			Expect(ac2).NotTo(BeNil(), "AC-2 parent not found")
			Expect(ac2.Class).To(Equal(oscal.ClassSection))
			Expect(ac2.Title).To(Equal("Account Management"))
		})

		It("emits AC-2.a as ClassRequirement with correct parent", func() {
			ac2a := findItem(items, "ac-2.a")
			Expect(ac2a).NotTo(BeNil(), "AC-2.a not found")
			Expect(ac2a.Class).To(Equal(oscal.ClassRequirement))
			Expect(ac2a.ParentID).To(Equal("ac-2"))
		})

		It("emits AC-2.c as ClassSection (has sub-items)", func() {
			ac2c := findItem(items, "ac-2.c")
			Expect(ac2c).NotTo(BeNil(), "AC-2.c not found")
			Expect(ac2c.Class).To(Equal(oscal.ClassSection))
		})

		It("emits AC-2.c.1 as ClassRequirement with parent AC-2.c", func() {
			ac2c1 := findItem(items, "ac-2.c.1")
			Expect(ac2c1).NotTo(BeNil(), "AC-2.c.1 not found")
			Expect(ac2c1.Class).To(Equal(oscal.ClassRequirement))
			Expect(ac2c1.ParentID).To(Equal("ac-2.c"))
		})
	})

	Context("parameter substitution", func() {
		BeforeEach(func() { parseMinimalCatalog() })

		It("substitutes catalog-level and control-level parameters in AC-1", func() {
			ac1 := findItem(items, "ac-1")
			Expect(ac1).NotTo(BeNil(), "AC-1 not found")

			Expect(ac1.Text).To(ContainSubstring("organization-defined personnel or roles"))
			Expect(ac1.Text).To(ContainSubstring("organization-defined frequency"))
			Expect(ac1.Text).NotTo(ContainSubstring("{{ insert:"))
		})
	})

	Context("sub-controls (enhancements)", func() {
		BeforeEach(func() { parseMinimalCatalog() })

		It("includes AC-2.1 with correct title and non-empty text", func() {
			ac21 := findItem(items, "ac-2.1")
			Expect(ac21).NotTo(BeNil(), "AC-2.1 not found")
			Expect(ac21.Title).To(Equal("Automated System Account Management"))
			Expect(ac21.Text).NotTo(BeEmpty())
		})
	})

	Context("guidance filtering", func() {
		BeforeEach(func() { parseMinimalCatalog() })

		It("excludes guidance text from AC-1", func() {
			ac1 := findItem(items, "ac-1")
			Expect(ac1).NotTo(BeNil(), "AC-1 not found")
			Expect(ac1.Text).NotTo(ContainSubstring("This is guidance text"))
		})
	})

	Context("error handling", func() {
		BeforeEach(func() {
			parser = oscal.NewParser("")
		})

		It("returns ErrNoControls for an empty catalog", func() {
			emptyDoc := `{
				"catalog": {
					"uuid": "00000000-0000-0000-0000-000000000000",
					"metadata": {
						"title": "Empty Catalog",
						"version": "1.0",
						"oscal-version": "1.1.3",
						"last-modified": "2026-06-11T00:00:00Z"
					}
				}
			}`
			_, err := parser.Parse(context.Background(), strings.NewReader(emptyDoc))
			Expect(err).To(MatchError(oscal.ErrNoControls))
		})

		It("returns a parse error for invalid JSON", func() {
			_, err := parser.Parse(context.Background(), strings.NewReader(`{ "catalog": { "uuid": "invalid`))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to parse OSCAL catalog"))
		})

		It("returns ErrInvalidFormat for a non-catalog document", func() {
			profileDoc := `{
				"profile": {
					"uuid": "00000000-0000-0000-0000-000000000000",
					"metadata": {
						"title": "Test Profile",
						"version": "1.0",
						"oscal-version": "1.1.3",
						"last-modified": "2026-06-11T00:00:00Z"
					}
				}
			}`
			_, err := parser.Parse(context.Background(), strings.NewReader(profileDoc))
			Expect(err).To(MatchError(oscal.ErrInvalidFormat))
		})
	})

	Describe("FindControl", func() {
		BeforeEach(func() { parseMinimalCatalog() })

		DescribeTable("locating controls by ID",
			func(controlID string, expectFound bool) {
				found, err := parser.FindControl(items, controlID)
				if expectFound {
					Expect(err).NotTo(HaveOccurred())
					Expect(found).NotTo(BeNil())
					Expect(found.ID).To(Equal(controlID))
				} else {
					Expect(err).To(HaveOccurred())
				}
			},
			Entry("finds existing control", "ac-2", true),
			Entry("finds sub-control", "ac-2.1", true),
			Entry("finds child item", "ac-2.a", true),
			Entry("returns error for nonexistent control", "nonexistent", false),
		)
	})
})

var _ = Describe("CleanProse", func() {
	DescribeTable("substitutes parameter values in prose text",
		func(text string, params map[string]string, want string) {
			Expect(oscal.CleanProse(text, params)).To(Equal(want))
		},
		Entry("basic substitution",
			"The system must use {{ insert: param, ac-1_prm_1 }} to authenticate.",
			map[string]string{"ac-1_prm_1": "multi-factor authentication"},
			"The system must use multi-factor authentication to authenticate.",
		),
		Entry("uses [param_id] fallback when param not found",
			"Configure {{ insert: param, missing_param }} before deployment.",
			map[string]string{"ac-1_prm_1": "value"},
			"Configure [missing_param] before deployment.",
		),
		Entry("replaces unrecognized template patterns with [parameter]",
			"This uses {{ unknown: template }} for configuration.",
			map[string]string{},
			"This uses [parameter] for configuration.",
		),
		Entry("handles multiple substitutions in one string",
			"Use {{ insert: param, auth_method }} for {{ insert: param, user_type }} users.",
			map[string]string{"auth_method": "PKI", "user_type": "privileged"},
			"Use PKI for privileged users.",
		),
		Entry("handles mixed found and missing parameters",
			"{{ insert: param, found }} and {{ insert: param, missing }} configured.",
			map[string]string{"found": "value1"},
			"value1 and [missing] configured.",
		),
		Entry("returns text unchanged when no templates present",
			"This is plain text without any templates.",
			map[string]string{},
			"This is plain text without any templates.",
		),
		Entry("handles param IDs with hyphens and dots",
			"{{ insert: param, ac-1.2_prm_3 }} is supported.",
			map[string]string{"ac-1.2_prm_3": "configured value"},
			"configured value is supported.",
		),
		Entry("handles templates with varying whitespace",
			"{{insert:param,no_space}} and {{ insert: param, with_space }} work.",
			map[string]string{"no_space": "value1", "with_space": "value2"},
			"value1 and value2 work.",
		),
		Entry("empty params map with template",
			"{{ insert: param, some_param }}",
			map[string]string{},
			"[some_param]",
		),
		Entry("empty text",
			"",
			map[string]string{"key": "value"},
			"",
		),
	)
})

var _ = Describe("CleanForEmbedding", func() {
	DescribeTable("cleans text for embedding by removing artifacts",
		func(text, want string) {
			Expect(oscal.CleanForEmbedding(text)).To(Equal(want))
		},
		Entry("strips OSCAL template markers",
			"This text has {{ insert: param, value }} embedded.",
			"This text has embedded.",
		),
		Entry("strips markdown table separators",
			"Header\n|------|------|\nData",
			"Header\nData",
		),
		Entry("strips VerDate metadata",
			"Text\nVerDate Sep 11 2014 12:30 Jul 01, 2024\nMore text",
			"Text\nMore text",
		),
		Entry("strips Jkt metadata",
			"Content\nJkt 123456 PO 00000\nNext line",
			"Content\nNext line",
		),
		Entry("strips PO metadata",
			"Line 1\nPO 12345 Fmt 8010\nLine 2",
			"Line 1\nLine 2",
		),
		Entry("strips Frm/Fmt/Sfmt metadata",
			"Text\nFrm 00001 Fmt 8010 Sfmt 8010\nMore",
			"Text\nMore",
		),
		Entry("strips Windows path metadata",
			"Content\nG:\\COMP\\SOME\\PATH\\FILE.XML\nNext",
			"Content\nNext",
		),
		Entry("collapses excessive newlines (3+ to 2)",
			"Line 1\n\n\n\nLine 2",
			"Line 1\n\nLine 2",
		),
		Entry("collapses excessive spaces (2+ to 1)",
			"Word1    Word2     Word3",
			"Word1 Word2 Word3",
		),
		Entry("trims leading and trailing whitespace",
			"  \n\n  Text content  \n\n  ",
			"Text content",
		),
		Entry("handles multiple OSCAL templates",
			"{{ insert: param, a }} and {{ insert: param, b }} text",
			"and text",
		),
		Entry("handles all PDF artifacts together",
			"Document start\n|------|------|\nVerDate Sep 11 2014 12:30 Jul 01, 2024\nJkt 123456 PO 00000\nPO 12345 Fmt 8010\nFrm 00001 Fmt 8010 Sfmt 8010\nG:\\COMP\\PATH\\FILE.XML\nDocument end",
			"Document start\nDocument end",
		),
		Entry("collapses four newlines to two",
			"A\n\n\n\nB",
			"A\n\nB",
		),
		Entry("collapses tabs and spaces",
			"Word1\t\t\tWord2  \t  Word3",
			"Word1 Word2 Word3",
		),
		Entry("preserves single newlines",
			"Line 1\nLine 2\nLine 3",
			"Line 1\nLine 2\nLine 3",
		),
		Entry("preserves double newlines",
			"Para 1\n\nPara 2",
			"Para 1\n\nPara 2",
		),
		Entry("empty string", "", ""),
		Entry("whitespace only",
			"   \n\n\n   ",
			"",
		),
		Entry("complex real-world example",
			"Control Statement\n\n{{ insert: param, ac-1_prm_1 }}\n\nThe organization must:\n\n|------|------|------|\na. Develop policies\nb. Review procedures\n\n\nVerDate Sep 11 2014 12:30 Jul 01, 2024\nJkt 123456 PO 00000 Frm 00001 Fmt 8010 Sfmt 8010\nG:\\COMP\\NIST\\SP800-53.XML\n\n\nFinal text.",
			"Control Statement\n\nThe organization must:\n\na. Develop policies\nb. Review procedures\n\nFinal text.",
		),
		Entry("strips all VerDate variations",
			"VerDate Mar 15 2010 09:45 Dec 31, 2023 Jkt 041481\nMore content",
			"More content",
		),
		Entry("strips nested templates (non-greedy match)",
			"{{ outer {{ inner }} }} text",
			"}} text",
		),
		Entry("handles mixed artifact types on same line",
			"Text\nJkt 123 PO 45678 Fmt 8010\nmore",
			"Text\nmore",
		),
	)
})

var _ = Describe("ValidateSchema", func() {
	var schemaPath string

	BeforeEach(func() {
		schemaPath = filepath.Join("..", "..", "schemas", "oscal", "1.2.2", "json-schema", "oscal_catalog_schema.json")
	})

	Context("when schema is available", func() {
		BeforeEach(func() {
			if _, err := os.Stat(schemaPath); os.IsNotExist(err) {
				Skip("OSCAL schema not fetched; run 'task dev:fetch-schemas' to download")
			}
		})

		It("passes validation for a valid OSCAL catalog", func() {
			data, err := os.ReadFile(filepath.Join("testdata", "minimal_catalog.json"))
			Expect(err).NotTo(HaveOccurred())

			err = oscal.ValidateSchema(data, schemaPath)
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns ErrInvalidFormat for malformed JSON input", func() {
			err := oscal.ValidateSchema([]byte(`{this is not valid json}`), schemaPath)
			Expect(err).To(HaveOccurred())
			Expect(err).To(MatchError(ContainSubstring(oscal.ErrInvalidFormat.Error())))
		})

		It("returns ErrValidationFailed for valid JSON that violates schema", func() {
			err := oscal.ValidateSchema([]byte(`{"catalog": {}}`), schemaPath)
			Expect(err).To(HaveOccurred())
			Expect(err).To(MatchError(ContainSubstring(oscal.ErrValidationFailed.Error())))
		})
	})

	Context("when schema is missing or malformed", func() {
		It("returns ErrSchemaLoad when schema file does not exist", func() {
			err := oscal.ValidateSchema(
				[]byte(`{"catalog": {"uuid": "test"}}`),
				filepath.Join("testdata", "nonexistent_schema.json"),
			)
			Expect(err).To(HaveOccurred())
			Expect(err).To(MatchError(ContainSubstring(oscal.ErrSchemaLoad.Error())))
		})

		It("returns ErrSchemaLoad for a malformed schema file", func() {
			tmpDir := GinkgoT().TempDir()
			badSchemaPath := filepath.Join(tmpDir, "bad_schema.json")
			Expect(os.WriteFile(badSchemaPath, []byte(`{not valid json`), 0o600)).To(Succeed())

			err := oscal.ValidateSchema(
				[]byte(`{"catalog": {"uuid": "test"}}`),
				badSchemaPath,
			)
			Expect(err).To(HaveOccurred())
			Expect(err).To(MatchError(ContainSubstring(oscal.ErrSchemaLoad.Error())))
		})
	})
})

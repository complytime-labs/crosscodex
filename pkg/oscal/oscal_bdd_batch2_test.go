package oscal_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	oscalTypes "github.com/defenseunicorns/go-oscal/src/types/oscal-1-1-3"

	"github.com/complytime-labs/crosscodex/pkg/oscal"
)

// loadTestCatalog loads the minimal_catalog.json test fixture for external tests.
func loadTestCatalog() *oscalTypes.Catalog {
	data, err := os.ReadFile("testdata/minimal_catalog.json")
	Expect(err).NotTo(HaveOccurred(), "failed to read test catalog")

	var schema oscalTypes.OscalCompleteSchema
	Expect(json.Unmarshal(data, &schema)).To(Succeed(), "failed to unmarshal test catalog")
	Expect(schema.Catalog).NotTo(BeNil(), "test catalog is nil")

	return schema.Catalog
}

// --- A5: Traversal ---

var _ = Describe("Traversal", func() {
	Describe("WalkControls", func() {
		It("visits all controls", func() {
			catalog := loadTestCatalog()

			visited := make(map[string]bool)
			visitor := func(ctrl oscalTypes.Control, groupID string, depth int) {
				visited[ctrl.ID] = true
			}

			oscal.WalkControls(catalog, visitor)

			expectedControls := []string{"ac-1", "ac-2", "ac-2.1", "ac-3"}
			for _, id := range expectedControls {
				Expect(visited).To(HaveKey(id), "control %s was not visited", id)
			}
			Expect(visited).To(HaveLen(len(expectedControls)))
		})

		It("passes correct group ID", func() {
			catalog := loadTestCatalog()

			groupIDs := make(map[string]string)
			visitor := func(ctrl oscalTypes.Control, groupID string, depth int) {
				groupIDs[ctrl.ID] = groupID
			}

			oscal.WalkControls(catalog, visitor)

			for _, controlID := range []string{"ac-1", "ac-2", "ac-2.1", "ac-3"} {
				Expect(groupIDs[controlID]).To(Equal("ac"),
					"control %s: expected groupID 'ac'", controlID)
			}
		})

		It("increments depth for nested controls", func() {
			catalog := loadTestCatalog()

			depths := make(map[string]int)
			visitor := func(ctrl oscalTypes.Control, groupID string, depth int) {
				depths[ctrl.ID] = depth
			}

			oscal.WalkControls(catalog, visitor)

			for _, controlID := range []string{"ac-1", "ac-2", "ac-3"} {
				Expect(depths[controlID]).To(Equal(0),
					"control %s: expected depth 0", controlID)
			}
			Expect(depths["ac-2.1"]).To(Equal(1), "control ac-2.1: expected depth 1")
		})

		It("handles nil catalog without panic", func() {
			Expect(func() {
				oscal.WalkControls(nil, func(ctrl oscalTypes.Control, groupID string, depth int) {
					Fail("visitor should not be called for nil catalog")
				})
			}).NotTo(Panic())
		})

		It("handles empty catalog without calling visitor", func() {
			catalog := &oscalTypes.Catalog{}

			visited := false
			visitor := func(ctrl oscalTypes.Control, groupID string, depth int) {
				visited = true
			}

			oscal.WalkControls(catalog, visitor)
			Expect(visited).To(BeFalse(), "visitor should not be called for empty catalog")
		})
	})

	Describe("FindControlInSlice", func() {
		It("finds control by exact ID", func() {
			items := []oscal.ControlItem{
				{ID: "ac-1", Title: "Access Control Policy"},
				{ID: "ac-2", Title: "Account Management"},
				{ID: "ac-3", Title: "Access Enforcement"},
			}

			item, err := oscal.FindControlInSlice(items, "ac-2")
			Expect(err).NotTo(HaveOccurred())
			Expect(item).NotTo(BeNil())
			Expect(item.ID).To(Equal("ac-2"))
			Expect(item.Title).To(Equal("Account Management"))

			// Verify we got a pointer to the actual item in the slice
			item.Title = "Modified"
			Expect(items[1].Title).To(Equal("Modified"),
				"expected pointer to slice element, got a copy")
		})

		It("returns ErrControlNotFound for missing ID", func() {
			items := []oscal.ControlItem{
				{ID: "ac-1", Title: "Access Control Policy"},
				{ID: "ac-2", Title: "Account Management"},
			}

			item, err := oscal.FindControlInSlice(items, "ac-99")
			Expect(item).To(BeNil())
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, oscal.ErrControlNotFound)).To(BeTrue())
			Expect(err.Error()).To(ContainSubstring("ac-99"))
		})

		It("rejects partial matches", func() {
			items := []oscal.ControlItem{
				{ID: "ac-1", Title: "Access Control Policy"},
				{ID: "ac-10", Title: "Concurrent Session Control"},
			}

			// "ac" should not match "ac-1" or "ac-10"
			item, err := oscal.FindControlInSlice(items, "ac")
			Expect(item).To(BeNil())
			Expect(err).To(HaveOccurred())

			// "ac-1" should match exactly and not "ac-10"
			item, err = oscal.FindControlInSlice(items, "ac-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(item).NotTo(BeNil())
			Expect(item.ID).To(Equal("ac-1"))
		})

		It("handles empty slice", func() {
			items := []oscal.ControlItem{}

			item, err := oscal.FindControlInSlice(items, "ac-1")
			Expect(item).To(BeNil())
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, oscal.ErrControlNotFound)).To(BeTrue())
		})
	})
})

// --- A6: Provenance ---

var _ = Describe("Provenance", func() {
	Describe("NewProvenance", func() {
		It("creates a provenance and tee reader", func() {
			content := "test content for hashing"
			reader := strings.NewReader(content)

			prov, teeReader, err := oscal.NewProvenance(reader)
			Expect(err).NotTo(HaveOccurred())
			Expect(prov).NotTo(BeNil())
			Expect(teeReader).NotTo(BeNil())

			Expect(time.Since(prov.RetrievalTimestamp)).To(BeNumerically("<", time.Second))
			Expect(prov.ContentHash).To(BeEmpty(), "ContentHash should be empty before reading")
			Expect(prov.ContentSize).To(BeZero(), "ContentSize should be zero before reading")
		})
	})

	Describe("TeeReader", func() {
		It("computes hash after full read", func() {
			content := "test content for hashing"
			reader := strings.NewReader(content)

			prov, teeReader, err := oscal.NewProvenance(reader)
			Expect(err).NotTo(HaveOccurred())

			data, err := io.ReadAll(teeReader)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(data)).To(Equal(content))

			hash := sha256.Sum256([]byte(content))
			expectedHash := hex.EncodeToString(hash[:])

			Expect(prov.ContentHash).To(Equal(expectedHash))
			Expect(prov.ContentSize).To(Equal(int64(len(content))))
		})

		It("handles empty content", func() {
			reader := strings.NewReader("")

			prov, teeReader, err := oscal.NewProvenance(reader)
			Expect(err).NotTo(HaveOccurred())

			data, err := io.ReadAll(teeReader)
			Expect(err).NotTo(HaveOccurred())
			Expect(data).To(BeEmpty())

			hash := sha256.Sum256([]byte(""))
			expectedHash := hex.EncodeToString(hash[:])

			Expect(prov.ContentHash).To(Equal(expectedHash))
			Expect(prov.ContentSize).To(BeZero())
		})

		It("handles large content (1MB)", func() {
			content := bytes.Repeat([]byte("a"), 1024*1024)
			reader := bytes.NewReader(content)

			prov, teeReader, err := oscal.NewProvenance(reader)
			Expect(err).NotTo(HaveOccurred())

			data, err := io.ReadAll(teeReader)
			Expect(err).NotTo(HaveOccurred())
			Expect(data).To(HaveLen(len(content)))

			hash := sha256.Sum256(content)
			expectedHash := hex.EncodeToString(hash[:])

			Expect(prov.ContentHash).To(Equal(expectedHash))
			Expect(prov.ContentSize).To(Equal(int64(len(content))))
		})

		It("computes correct hash from partial reads", func() {
			content := "test content for partial reading"
			reader := strings.NewReader(content)

			prov, teeReader, err := oscal.NewProvenance(reader)
			Expect(err).NotTo(HaveOccurred())

			// Read in chunks
			buf := make([]byte, 5)
			var total int
			for {
				n, readErr := teeReader.Read(buf)
				total += n
				if readErr == io.EOF {
					break
				}
				Expect(readErr).NotTo(HaveOccurred())
			}

			Expect(total).To(Equal(len(content)))

			hash := sha256.Sum256([]byte(content))
			expectedHash := hex.EncodeToString(hash[:])

			Expect(prov.ContentHash).To(Equal(expectedHash))
			Expect(prov.ContentSize).To(Equal(int64(len(content))))
		})
	})

	Describe("SetExtractor", func() {
		It("sets extractor name and version", func() {
			prov := &oscal.Provenance{}

			prov.SetExtractor("TestExtractor", "v1.2.3")

			Expect(prov.ExtractorName).To(Equal("TestExtractor"))
			Expect(prov.ExtractorVersion).To(Equal("v1.2.3"))
		})
	})

	Describe("SetOutputHash", func() {
		It("computes hash of output data", func() {
			prov := &oscal.Provenance{}
			data := []byte("output data")

			prov.SetOutputHash(data)

			hash := sha256.Sum256(data)
			expectedHash := hex.EncodeToString(hash[:])
			Expect(prov.OutputHash).To(Equal(expectedHash))
		})

		It("computes hash of empty data", func() {
			prov := &oscal.Provenance{}
			data := []byte("")

			prov.SetOutputHash(data)

			hash := sha256.Sum256(data)
			expectedHash := hex.EncodeToString(hash[:])
			Expect(prov.OutputHash).To(Equal(expectedHash))
		})
	})

	Describe("Full Workflow", func() {
		It("populates all fields through provenance lifecycle", func() {
			inputContent := "source document content"
			outputContent := "extracted OSCAL JSON"

			reader := strings.NewReader(inputContent)
			prov, teeReader, err := oscal.NewProvenance(reader)
			Expect(err).NotTo(HaveOccurred())

			prov.SourceURI = "file:///path/to/source.pdf"
			prov.Format = "pdf"

			_, err = io.ReadAll(teeReader)
			Expect(err).NotTo(HaveOccurred())

			prov.SetExtractor("PDFExtractor", "v2.1.0")
			prov.SetOutputHash([]byte(outputContent))

			Expect(prov.SourceURI).To(Equal("file:///path/to/source.pdf"))
			Expect(prov.Format).To(Equal("pdf"))
			Expect(prov.ContentHash).NotTo(BeEmpty())
			Expect(prov.ContentSize).To(Equal(int64(len(inputContent))))
			Expect(prov.OutputHash).NotTo(BeEmpty())
			Expect(prov.ExtractorName).To(Equal("PDFExtractor"))
			Expect(prov.ExtractorVersion).To(Equal("v2.1.0"))
			Expect(time.Since(prov.RetrievalTimestamp)).To(BeNumerically("<", time.Second))
		})
	})

	Describe("String", func() {
		It("includes all key fields in string representation", func() {
			prov := &oscal.Provenance{
				SourceURI:          "file:///test.pdf",
				RetrievalTimestamp: time.Now(),
				ContentHash:        "abc123",
				ContentSize:        1024,
				Format:             "pdf",
				OutputHash:         "def456",
				ExtractorName:      "TestExtractor",
				ExtractorVersion:   "v1.0.0",
			}

			str := prov.String()

			Expect(str).To(ContainSubstring("file:///test.pdf"))
			Expect(str).To(ContainSubstring("abc123"))
			Expect(str).To(ContainSubstring("1024"))
			Expect(str).To(ContainSubstring("pdf"))
			Expect(str).To(ContainSubstring("def456"))
			Expect(str).To(ContainSubstring("TestExtractor"))
			Expect(str).To(ContainSubstring("v1.0.0"))
		})
	})
})

// --- A7: Convert ---

var _ = Describe("Convert", func() {
	Describe("ControlToItem", func() {
		It("converts basic fields correctly", func() {
			ctrl := oscalTypes.Control{
				ID:    "ac-1",
				Title: "Policy and Procedures",
			}

			item := oscal.ControlToItem(ctrl, "ac", nil)

			Expect(item.ID).To(Equal("ac-1"))
			Expect(item.Title).To(Equal("Policy and Procedures"))
			Expect(item.GroupID).To(Equal("ac"))
			Expect(item.Class).To(Equal(oscal.ClassRequirement))
			Expect(item.Props).NotTo(BeNil())
		})

		It("extracts statement prose only, not guidance", func() {
			parts := []oscalTypes.Part{
				{Name: "guidance", Prose: "This is guidance text that should be ignored."},
				{Name: "statement", Prose: "This is the control statement."},
				{Name: "other", Prose: "This should also be ignored."},
			}

			ctrl := oscalTypes.Control{
				ID:    "ac-2",
				Title: "Account Management",
				Parts: &parts,
			}

			item := oscal.ControlToItem(ctrl, "ac", nil)
			Expect(item.Text).To(Equal("This is the control statement."))
		})

		It("handles control with no parts gracefully", func() {
			ctrl := oscalTypes.Control{
				ID:    "ac-3",
				Title: "Access Enforcement",
				Parts: nil,
			}

			item := oscal.ControlToItem(ctrl, "ac", nil)
			Expect(item.Text).To(BeEmpty())
			Expect(item.Class).To(Equal(oscal.ClassRequirement))
		})

		It("recursively extracts nested statement prose", func() {
			subParts := []oscalTypes.Part{
				{Name: "item", Prose: "Sub-requirement a."},
				{Name: "item", Prose: "Sub-requirement b."},
			}
			parts := []oscalTypes.Part{
				{
					Name:  "statement",
					Prose: "Main requirement:",
					Parts: &subParts,
				},
			}

			ctrl := oscalTypes.Control{
				ID:    "ac-4",
				Title: "Information Flow Enforcement",
				Parts: &parts,
			}

			item := oscal.ControlToItem(ctrl, "ac", nil)
			Expect(item.Text).To(Equal("Main requirement:\nSub-requirement a.\nSub-requirement b."))
		})

		It("applies parameter substitution when params provided", func() {
			parts := []oscalTypes.Part{
				{
					Name:  "statement",
					Prose: "Review access every {{ insert: param, ac-2_prm_1 }} days.",
				},
			}

			ctrl := oscalTypes.Control{
				ID:    "ac-5",
				Title: "Separation of Duties",
				Parts: &parts,
			}

			params := map[string]string{
				"ac-2_prm_1": "90",
			}

			item := oscal.ControlToItem(ctrl, "ac", params)
			Expect(item.Text).To(Equal("Review access every 90 days."))
		})
	})

	Describe("ItemToControl", func() {
		It("round-trips ID and Title correctly", func() {
			item := oscal.ControlItem{
				ID:      "ac-6",
				Title:   "Least Privilege",
				Text:    "Employ least privilege principle.",
				GroupID: "ac",
				Class:   oscal.ClassRequirement,
				Props:   make(map[string]string),
			}

			ctrl := oscal.ItemToControl(item)

			Expect(ctrl.ID).To(Equal("ac-6"))
			Expect(ctrl.Title).To(Equal("Least Privilege"))
			Expect(ctrl.Parts).NotTo(BeNil())
			Expect(*ctrl.Parts).To(HaveLen(1))

			part := (*ctrl.Parts)[0]
			Expect(part.Name).To(Equal("statement"))
			Expect(part.Prose).To(Equal("Employ least privilege principle."))
		})

		It("sets compliance-mapper namespace on props", func() {
			item := oscal.ControlItem{
				ID:    "ac-7",
				Title: "Unsuccessful Logon Attempts",
				Props: map[string]string{
					"custom-key":   "custom-value",
					"another-prop": "another-value",
				},
			}

			ctrl := oscal.ItemToControl(item)

			Expect(ctrl.Props).NotTo(BeNil())
			props := *ctrl.Props
			Expect(props).To(HaveLen(2))

			for _, prop := range props {
				Expect(prop.Ns).To(Equal(oscal.ExportComplianceMapperNS))
				if prop.Name == "custom-key" {
					Expect(prop.Value).To(Equal("custom-value"))
				}
				if prop.Name == "another-prop" {
					Expect(prop.Value).To(Equal("another-value"))
				}
			}
		})

		It("handles empty text by producing nil parts", func() {
			item := oscal.ControlItem{
				ID:    "ac-13",
				Title: "Supervision and Review - Access Control",
				Text:  "",
				Props: make(map[string]string),
			}

			ctrl := oscal.ItemToControl(item)
			Expect(ctrl.Parts).To(BeNil())
		})

		It("handles empty props by producing nil props", func() {
			item := oscal.ControlItem{
				ID:    "ac-14",
				Title: "Permitted Actions Without Identification",
				Text:  "Some text.",
				Props: make(map[string]string),
			}

			ctrl := oscal.ItemToControl(item)
			Expect(ctrl.Props).To(BeNil())
		})
	})

	Describe("CollectParams", func() {
		It("merges catalog and control params with control overriding catalog", func() {
			catalogParams := []oscalTypes.Parameter{
				{ID: "param-1", Label: "Catalog Default"},
				{ID: "param-2", Values: &[]string{"catalog-value"}},
			}

			controlParams := []oscalTypes.Parameter{
				{ID: "param-1", Label: "Control Override"},
				{ID: "param-3", Label: "Control Only"},
			}

			catalog := &oscalTypes.Catalog{
				UUID:   "test-catalog",
				Params: &catalogParams,
			}

			ctrl := oscalTypes.Control{
				ID:     "ac-8",
				Title:  "System Use Notification",
				Params: &controlParams,
			}

			params := oscal.CollectParams(catalog, ctrl)

			Expect(params).To(HaveLen(3))
			Expect(params["param-1"]).To(Equal("Control Override"))
			Expect(params["param-2"]).To(Equal("catalog-value"))
			Expect(params["param-3"]).To(Equal("Control Only"))
		})

		It("prefers Label over Values", func() {
			catalogParams := []oscalTypes.Parameter{
				{
					ID:     "param-1",
					Label:  "Preferred Label",
					Values: &[]string{"fallback-value"},
				},
			}

			catalog := &oscalTypes.Catalog{
				UUID:   "test-catalog",
				Params: &catalogParams,
			}

			ctrl := oscalTypes.Control{
				ID:    "ac-9",
				Title: "Previous Logon Notification",
			}

			params := oscal.CollectParams(catalog, ctrl)
			Expect(params["param-1"]).To(Equal("Preferred Label"))
		})

		It("handles nil catalog gracefully", func() {
			controlParams := []oscalTypes.Parameter{
				{ID: "param-1", Label: "Control Param"},
			}

			ctrl := oscalTypes.Control{
				ID:     "ac-11",
				Title:  "Device Lock",
				Params: &controlParams,
			}

			params := oscal.CollectParams(nil, ctrl)
			Expect(params).To(HaveLen(1))
			Expect(params["param-1"]).To(Equal("Control Param"))
		})
	})

	Describe("Round-trip", func() {
		It("preserves key fields through ControlToItem → ItemToControl", func() {
			parts := []oscalTypes.Part{
				{Name: "statement", Prose: "Original control statement."},
			}
			originalCtrl := oscalTypes.Control{
				ID:    "ac-10",
				Title: "Concurrent Session Control",
				Parts: &parts,
			}

			item := oscal.ControlToItem(originalCtrl, "ac", nil)
			roundTripCtrl := oscal.ItemToControl(item)

			Expect(roundTripCtrl.ID).To(Equal(originalCtrl.ID))
			Expect(roundTripCtrl.Title).To(Equal(originalCtrl.Title))
			Expect(roundTripCtrl.Parts).NotTo(BeNil())
			Expect(*roundTripCtrl.Parts).NotTo(BeEmpty())

			roundTripPart := (*roundTripCtrl.Parts)[0]
			originalPart := (*originalCtrl.Parts)[0]
			Expect(roundTripPart.Prose).To(Equal(originalPart.Prose))
		})
	})

	Describe("extractStatementProse", func() {
		It("handles deeply nested parts", func() {
			level3 := []oscalTypes.Part{
				{Name: "item", Prose: "Level 3 requirement."},
			}
			level2 := []oscalTypes.Part{
				{Name: "item", Prose: "Level 2 requirement.", Parts: &level3},
			}
			level1 := []oscalTypes.Part{
				{
					Name:  "statement",
					Prose: "Level 1 requirement.",
					Parts: &level2,
				},
			}

			ctrl := oscalTypes.Control{
				ID:    "ac-12",
				Title: "Multi-level Control",
				Parts: &level1,
			}

			text := oscal.ExportExtractStatementProse(ctrl)
			Expect(text).To(Equal("Level 1 requirement.\nLevel 2 requirement.\nLevel 3 requirement."))
		})
	})
})

// --- A8: Decompose ---

var _ = Describe("Decompose", func() {
	Describe("DecomposeStatements", func() {
		It("decomposes a control with statement items into parent + children", func() {
			ctrl := oscalTypes.Control{
				ID:    "ac-2",
				Title: "Account Management",
				Parts: &[]oscalTypes.Part{
					{
						ID:    "ac-2_smt",
						Name:  "statement",
						Prose: "The organization shall:",
						Parts: &[]oscalTypes.Part{
							{
								ID:    "ac-2_smt.a",
								Name:  "item",
								Prose: "Identify and select account types.",
							},
							{
								ID:    "ac-2_smt.b",
								Name:  "item",
								Prose: "Assign account managers.",
							},
						},
					},
				},
			}

			params := make(map[string]string)
			result := oscal.DecomposeStatements(ctrl, "access-control", params)

			Expect(result).To(HaveLen(3))

			// Parent
			parent := result[0]
			Expect(parent.ID).To(Equal("ac-2"))
			Expect(parent.Class).To(Equal(oscal.ClassSection))
			Expect(parent.ParentID).To(BeEmpty())
			Expect(parent.Text).To(Equal("The organization shall:"))

			// Child 1
			child1 := result[1]
			Expect(child1.ID).To(Equal("ac-2.a"))
			Expect(child1.Class).To(Equal(oscal.ClassRequirement))
			Expect(child1.ParentID).To(Equal("ac-2"))
			Expect(child1.Props["parent-id"]).To(Equal("ac-2"))

			// Child 2
			child2 := result[2]
			Expect(child2.ID).To(Equal("ac-2.b"))
			Expect(child2.Class).To(Equal(oscal.ClassRequirement))
		})

		It("handles nested sub-items", func() {
			ctrl := oscalTypes.Control{
				ID:    "ac-3",
				Title: "Access Enforcement",
				Parts: &[]oscalTypes.Part{
					{
						ID:    "ac-3_smt",
						Name:  "statement",
						Prose: "The system enforces approved authorizations for:",
						Parts: &[]oscalTypes.Part{
							{
								ID:    "ac-3_smt.a",
								Name:  "item",
								Prose: "Logical access",
								Parts: &[]oscalTypes.Part{
									{
										ID:    "ac-3_smt.a.1",
										Name:  "item",
										Prose: "Read access to system files",
									},
									{
										ID:    "ac-3_smt.a.2",
										Name:  "item",
										Prose: "Write access to system files",
									},
								},
							},
						},
					},
				},
			}

			params := make(map[string]string)
			result := oscal.DecomposeStatements(ctrl, "access-control", params)

			Expect(result).To(HaveLen(4))

			Expect(result[0].ID).To(Equal("ac-3"))
			Expect(result[0].Class).To(Equal(oscal.ClassSection))

			itemA := result[1]
			Expect(itemA.ID).To(Equal("ac-3.a"))
			Expect(itemA.Class).To(Equal(oscal.ClassSection))
			Expect(itemA.ParentID).To(Equal("ac-3"))

			subItem1 := result[2]
			Expect(subItem1.ID).To(Equal("ac-3.a.1"))
			Expect(subItem1.Class).To(Equal(oscal.ClassRequirement))
			Expect(subItem1.ParentID).To(Equal("ac-3.a"))

			subItem2 := result[3]
			Expect(subItem2.ID).To(Equal("ac-3.a.2"))
			Expect(subItem2.Class).To(Equal(oscal.ClassRequirement))
		})

		It("returns single requirement item when no statement items exist", func() {
			ctrl := oscalTypes.Control{
				ID:    "ac-1",
				Title: "Policy and Procedures",
				Parts: &[]oscalTypes.Part{
					{
						ID:    "ac-1_desc",
						Name:  "description",
						Prose: "The organization develops and implements access control policies.",
					},
				},
			}

			params := make(map[string]string)
			result := oscal.DecomposeStatements(ctrl, "access-control", params)

			Expect(result).To(HaveLen(1))
			Expect(result[0].ID).To(Equal("ac-1"))
			Expect(result[0].Class).To(Equal(oscal.ClassRequirement))
			Expect(result[0].ParentID).To(BeEmpty())
		})

		Context("ID derivation", func() {
			It("strips _smt and _stmt suffixes", func() {
				ctrl := oscalTypes.Control{
					ID:    "ac-2",
					Title: "Account Management",
					Parts: &[]oscalTypes.Part{
						{
							ID:    "ac-2_smt",
							Name:  "statement",
							Prose: "The organization shall:",
							Parts: &[]oscalTypes.Part{
								{
									ID:    "ac-2_smt.a",
									Name:  "item",
									Prose: "First item",
								},
								{
									ID:    "ac-2_stmt.b",
									Name:  "item",
									Prose: "Second item",
								},
							},
						},
					},
				}

				params := make(map[string]string)
				result := oscal.DecomposeStatements(ctrl, "access-control", params)

				Expect(result[1].ID).To(Equal("ac-2.a"))
				Expect(result[2].ID).To(Equal("ac-2.b"))
			})

			It("uses fallback ID when part ID is empty or non-standard", func() {
				ctrl := oscalTypes.Control{
					ID:    "ac-4",
					Title: "Information Flow Enforcement",
					Parts: &[]oscalTypes.Part{
						{
							ID:    "ac-4_smt",
							Name:  "statement",
							Prose: "The system enforces:",
							Parts: &[]oscalTypes.Part{
								{
									ID:    "",
									Name:  "item",
									Prose: "First item",
								},
								{
									ID:    "custom-id",
									Name:  "item",
									Prose: "Second item",
								},
							},
						},
					},
				}

				params := make(map[string]string)
				result := oscal.DecomposeStatements(ctrl, "access-control", params)

				Expect(result[1].ID).To(Equal("ac-4.1"))
				Expect(result[2].ID).To(Equal("ac-4.2"))
			})
		})
	})

	Describe("DecomposeText", func() {
		It("decomposes parenthesized clauses", func() {
			text := "(a) First requirement here.\n(b) Second requirement here.\n(c) Third requirement here."
			result := oscal.DecomposeText("test-1", text, 5)

			Expect(result).To(HaveLen(3))

			expectedIDs := []string{"test-1.a", "test-1.b", "test-1.c"}
			for i, item := range result {
				Expect(item.ID).To(Equal(expectedIDs[i]))
				Expect(item.Class).To(Equal(oscal.ClassRequirement))
				Expect(item.Props["label"]).To(Equal(string(expectedIDs[i][len(expectedIDs[i])-1])))
			}
		})

		It("decomposes numbered paragraphs", func() {
			text := "1. This is the first numbered paragraph with enough words to meet minimum.\n" +
				"2. This is the second numbered paragraph with enough words to meet minimum.\n" +
				"3. This is the third numbered paragraph with enough words to meet minimum."

			result := oscal.DecomposeText("test-2", text, 8)

			Expect(result).To(HaveLen(3))

			expectedIDs := []string{"test-2.1", "test-2.2", "test-2.3"}
			for i, item := range result {
				Expect(item.ID).To(Equal(expectedIDs[i]))
			}
		})

		It("returns single item when text is below minimum word count", func() {
			text := "Short text"
			result := oscal.DecomposeText("test-3", text, 10)

			Expect(result).To(HaveLen(1))
			Expect(result[0].ID).To(Equal("test-3"))
			Expect(result[0].Class).To(Equal(oscal.ClassRequirement))
		})

		It("handles recursive decomposition with nested levels", func() {
			text := "(a) First item has these sub-parts:\n" +
				"1. Sub-requirement one with enough words to trigger decomposition here.\n" +
				"2. Sub-requirement two with enough words to trigger decomposition here.\n" +
				"(b) Second item is simple enough and has enough words."

			result := oscal.DecomposeText("test-4", text, 5)

			Expect(len(result)).To(BeNumerically(">=", 4))

			// First item should be a section (has children)
			Expect(result[0].Class).To(Equal(oscal.ClassSection))

			// Child references parent
			Expect(result[1].ParentID).To(Equal(result[0].ID))
		})
	})

	Describe("wordCount", func() {
		DescribeTable("counts words correctly",
			func(text string, expected int) {
				Expect(oscal.ExportWordCount(text)).To(Equal(expected))
			},
			Entry("empty string", "", 0),
			Entry("single word", "one", 1),
			Entry("three words", "one two three", 3),
			Entry("multiple spaces", "  multiple   spaces   between  ", 3),
			Entry("newlines", "newline\nwords\nhere", 3),
			Entry("tabs and spaces mixed", "tabs\tand\tspaces  mixed", 4),
		)
	})

	Describe("deriveChildID", func() {
		DescribeTable("derives child IDs correctly",
			func(parentID string, partID string, index int, expected string) {
				part := oscalTypes.Part{ID: partID}
				Expect(oscal.ExportDeriveChildID(parentID, part, index)).To(Equal(expected))
			},
			Entry("strips _smt suffix", "ac-2", "ac-2_smt.a", 0, "ac-2.a"),
			Entry("strips _stmt suffix", "ac-2", "ac-2_stmt.a", 0, "ac-2.a"),
			Entry("keeps dot-suffix as-is", "ac-2", "ac-2.a", 0, "ac-2.a"),
			Entry("uses fallback for empty ID", "ac-2", "", 0, "ac-2.1"),
			Entry("uses index-based fallback for non-standard ID", "ac-2", "custom-id", 1, "ac-2.2"),
			Entry("handles nested parent ID", "ac-3.a", "ac-3.a_smt.1", 0, "ac-3.a.1"),
		)
	})
})

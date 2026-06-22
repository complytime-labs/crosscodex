package oscal_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/oscal"
	"github.com/complytime-labs/crosscodex/pkg/prompt"
)

// testCompleter implements oscal.Completer for testing.
type testCompleter struct {
	response string
	err      error
}

// failingRegistry implements prompt.Registry but always fails on Render.
// Used to test the fallback path when registry.Render returns an error.
type failingRegistry struct{}

func (f *failingRegistry) Resolve(_ context.Context, _ string) (*prompt.PromptSpec, error) {
	return nil, errors.New("intentional test failure")
}

func (f *failingRegistry) Render(_ context.Context, _ string, _ map[string]string) (*prompt.ResolvedPrompt, error) {
	return nil, errors.New("intentional test failure")
}

func (f *failingRegistry) List(_ context.Context) ([]string, error) {
	return nil, errors.New("intentional test failure")
}

func (f *failingRegistry) Layers(_ context.Context, _ string) ([]prompt.LayerInfo, error) {
	return nil, errors.New("intentional test failure")
}

func (tc *testCompleter) Complete(_ context.Context, _ []oscal.Message) (string, error) {
	return tc.response, tc.err
}

// testRegistry creates a minimal prompt.Registry for testing with optional system prompt override.
func testRegistry(overrides map[string]string) prompt.Registry {
	cfg := config.PromptConfig{
		Layers: config.PromptLayerConfig{Enabled: true},
	}

	if len(overrides) == 0 {
		reg, err := prompt.NewRegistry(cfg)
		if err != nil {
			panic(err)
		}
		return reg
	}

	// Create a temp directory with overlay prompts
	tmpDir, err := os.MkdirTemp("", "oscal-test-*")
	if err != nil {
		panic(err)
	}
	promptDir := filepath.Join(tmpDir, ".crosscodex", "prompts")
	if err := os.MkdirAll(promptDir, 0o755); err != nil {
		panic(err)
	}

	for name, systemPrompt := range overrides {
		yamlContent := fmt.Sprintf("name: %s\nversion: 1.0.0\ntemplates:\n  system: %q\n  user: \"${document_chunk}\"\n", name, systemPrompt)
		if err := os.WriteFile(filepath.Join(promptDir, name+".yaml"), []byte(yamlContent), 0o644); err != nil {
			panic(err)
		}
	}

	reg, err := prompt.NewRegistry(cfg, prompt.WithProjectDir(tmpDir))
	if err != nil {
		panic(err)
	}
	return reg
}

var _ = Describe("TierPattern", func() {
	var opts oscal.StructureOptions

	BeforeEach(func() {
		opts = oscal.StructureOptions{}
	})

	Context("when no pattern matches", func() {
		It("returns false and nil items", func() {
			doc := oscal.StructuredDoc{
				RawText: "Just some random text\nwith no discernible pattern\nat all",
			}
			items, ok := oscal.TierPattern(doc, opts)
			Expect(ok).To(BeFalse())
			Expect(items).To(BeNil())
		})
	})

	Context("when fewer than 3 matches", func() {
		It("returns false and nil items", func() {
			doc := oscal.StructuredDoc{
				RawText: "1. First item\n2. Second item\nSome random text",
			}
			items, ok := oscal.TierPattern(doc, opts)
			Expect(ok).To(BeFalse())
			Expect(items).To(BeNil())
		})
	})

	Context("when the first matching pattern has priority", func() {
		It("selects the higher-priority pattern", func() {
			doc := oscal.StructuredDoc{
				RawText: "1.1 Higher priority pattern\n1.2 Should match this\n1.3 Not the lower priority\n1. This would match pattern 12 but pattern 1 has priority",
			}
			items, ok := oscal.TierPattern(doc, opts)
			Expect(ok).To(BeTrue())
			Expect(items).To(HaveLen(3))
			Expect(items[0].ID).To(Equal("1.1"))
		})
	})

	DescribeTable("detecting numbered/labelled patterns",
		func(rawText string, expectedLen int, expectedIDs []string) {
			doc := oscal.StructuredDoc{RawText: rawText}
			items, ok := oscal.TierPattern(doc, opts)
			Expect(ok).To(BeTrue())
			Expect(items).To(HaveLen(expectedLen))
			for i, expID := range expectedIDs {
				Expect(items[i].ID).To(Equal(expID), "item %d ID mismatch", i)
			}
		},
		Entry("pattern 1: numeric dot notation (1.1, 2.3)",
			"1.1 First requirement\n1.2 Second requirement\n2.1 Third requirement",
			3, []string{"1.1", "1.2", "2.1"},
		),
		Entry("pattern 2: letter dot number (A.1, B.2)",
			"A.1 First rule\nA.2 Second rule\nB.1 Third rule",
			3, []string{"A.1", "A.2", "B.1"},
		),
		Entry("pattern 3: Article N",
			"Article 1 - Data Protection\nArticle 2 - Privacy Rights\nArticle 3 - User Consent",
			3, []string{"Article 1", "Article 2", "Article 3"},
		),
		Entry("pattern 4: Section N",
			"Section 1 Introduction\nSection 2 Requirements\nSection 3 Compliance",
			3, nil,
		),
		Entry("pattern 5: Rule N",
			"Rule 1 Authentication required\nRule 2 Authorization required\nRule 3 Audit logging required",
			3, nil,
		),
		Entry("pattern 6: section symbol (§1, § 2)",
			"§1 Legal requirement\n§2 Regulatory compliance\n§ 3 Audit trail",
			3, nil,
		),
		Entry("pattern 7: lowercase letter in parens (a), (b)",
			"(a) First clause\n(b) Second clause\n(c) Third clause",
			3, []string{"(a)", "(b)", "(c)"},
		),
		Entry("pattern 8: number in parens (1), (2)",
			"(1) First item\n(2) Second item\n(3) Third item",
			3, nil,
		),
		Entry("pattern 9: roman numerals (i., ii., iii.)",
			"i. First point\nii. Second point\niii. Third point",
			3, nil,
		),
		Entry("pattern 10: lowercase letter dot (a., b., c.)",
			"a. Alpha requirement\nb. Beta requirement\nc. Gamma requirement",
			3, nil,
		),
		Entry("pattern 11: uppercase letter dot (A., B., C.)",
			"A. First requirement\nB. Second requirement\nC. Third requirement",
			3, nil,
		),
		Entry("pattern 12: simple number dot (1., 2., 3.)",
			"1. First item\n2. Second item\n3. Third item",
			3, nil,
		),
	)

	It("sets Class to ClassRequirement on matched items", func() {
		doc := oscal.StructuredDoc{
			RawText: "1.1 First requirement\n1.2 Second requirement\n2.1 Third requirement",
		}
		items, ok := oscal.TierPattern(doc, opts)
		Expect(ok).To(BeTrue())
		for i, item := range items {
			Expect(item.Class).To(Equal(oscal.ClassRequirement), "item %d Class mismatch", i)
		}
	})
})

var _ = Describe("TierRegex", func() {
	Context("when SectionPattern is empty", func() {
		It("returns false and nil items", func() {
			doc := oscal.StructuredDoc{RawText: "some text"}
			opts := oscal.StructureOptions{SectionPattern: ""}
			items, ok := oscal.TierRegex(doc, opts)
			Expect(ok).To(BeFalse())
			Expect(items).To(BeNil())
		})
	})

	Context("when regex is invalid", func() {
		It("returns false and nil items", func() {
			doc := oscal.StructuredDoc{RawText: "some text"}
			opts := oscal.StructureOptions{SectionPattern: "[invalid(regex"}
			items, ok := oscal.TierRegex(doc, opts)
			Expect(ok).To(BeFalse())
			Expect(items).To(BeNil())
		})
	})

	Context("when no matches found", func() {
		It("returns false and nil items", func() {
			doc := oscal.StructuredDoc{RawText: "no matches here"}
			opts := oscal.StructureOptions{SectionPattern: `^SECTION:\s*(.+)`}
			items, ok := oscal.TierRegex(doc, opts)
			Expect(ok).To(BeFalse())
			Expect(items).To(BeNil())
		})
	})

	Context("when pattern is oversized", func() {
		It("rejects patterns exceeding 512 characters", func() {
			doc := oscal.StructuredDoc{RawText: "1.1 First\n1.2 Second\n"}
			longPattern := strings.Repeat("a", 513)
			items, ok := oscal.TierRegex(doc, oscal.StructureOptions{SectionPattern: longPattern})
			Expect(ok).To(BeFalse())
			Expect(items).To(BeNil())
		})
	})

	Context("with two capture groups (ID and text)", func() {
		It("extracts ID from first group and text from second", func() {
			doc := oscal.StructuredDoc{
				RawText: "AC-1: Access Control Policy\nAC-2: Account Management\nAC-3: Access Enforcement",
			}
			opts := oscal.StructureOptions{SectionPattern: `^([A-Z]+-\d+):\s*(.+)`}
			items, ok := oscal.TierRegex(doc, opts)
			Expect(ok).To(BeTrue())
			Expect(items).To(HaveLen(3))

			Expect(items[0].ID).To(Equal("AC-1"))
			Expect(items[0].Text).To(Equal("Access Control Policy"))
			Expect(items[0].Class).To(Equal(oscal.ClassRequirement))

			Expect(items[1].ID).To(Equal("AC-2"))
			Expect(items[1].Text).To(Equal("Account Management"))

			Expect(items[2].ID).To(Equal("AC-3"))
			Expect(items[2].Text).To(Equal("Access Enforcement"))
		})
	})

	Context("with one capture group", func() {
		It("auto-generates IDs and uses capture as text", func() {
			doc := oscal.StructuredDoc{
				RawText: "REQUIREMENT: Must authenticate users\nREQUIREMENT: Must encrypt data\nREQUIREMENT: Must log access",
			}
			opts := oscal.StructureOptions{SectionPattern: `REQUIREMENT:\s*(.+)`}
			items, ok := oscal.TierRegex(doc, opts)
			Expect(ok).To(BeTrue())
			Expect(items).To(HaveLen(3))

			Expect(items[0].ID).To(Equal("sec-1"))
			Expect(items[0].Text).To(Equal("Must authenticate users"))

			Expect(items[1].ID).To(Equal("sec-2"))
			Expect(items[1].Text).To(Equal("Must encrypt data"))

			Expect(items[2].ID).To(Equal("sec-3"))
			Expect(items[2].Text).To(Equal("Must log access"))
		})
	})

	Context("with no capture groups", func() {
		It("uses full match as text with auto-generated IDs", func() {
			doc := oscal.StructuredDoc{
				RawText: "1.1 First requirement\n2.2 Second requirement",
			}
			opts := oscal.StructureOptions{SectionPattern: `\d+\.\d+\s+\w+\s+\w+`}
			items, ok := oscal.TierRegex(doc, opts)
			Expect(ok).To(BeTrue())
			Expect(items).To(HaveLen(2))
			Expect(items[0].ID).To(Equal("sec-1"))
			Expect(items[0].Text).To(Equal("1.1 First requirement"))
		})
	})
})

var _ = Describe("TierTable", func() {
	var opts oscal.StructureOptions

	BeforeEach(func() {
		opts = oscal.StructureOptions{}
	})

	Context("when no tables are present", func() {
		It("returns false and nil items", func() {
			doc := oscal.StructuredDoc{Tables: []oscal.DocTable{}}
			items, ok := oscal.TierTable(doc, opts)
			Expect(ok).To(BeFalse())
			Expect(items).To(BeNil())
		})
	})

	Context("when all rows are empty", func() {
		It("returns false and nil items", func() {
			doc := oscal.StructuredDoc{
				Tables: []oscal.DocTable{
					{
						Headers: []string{"ID", "Desc"},
						Rows:    [][]string{{}, {}},
					},
				},
			}
			items, ok := oscal.TierTable(doc, opts)
			Expect(ok).To(BeFalse())
			Expect(items).To(BeNil())
		})
	})

	Context("when tables contain valid rows", func() {
		It("extracts rows with ID from first column", func() {
			doc := oscal.StructuredDoc{
				Tables: []oscal.DocTable{
					{
						Headers: []string{"ID", "Description", "Status"},
						Rows: [][]string{
							{"AC-1", "Access Control Policy", "Active"},
							{"AC-2", "Account Management", "Active"},
						},
					},
				},
			}
			items, ok := oscal.TierTable(doc, opts)
			Expect(ok).To(BeTrue())
			Expect(items).To(HaveLen(2))

			Expect(items[0].ID).To(Equal("AC-1"))
			Expect(items[0].Text).To(Equal("AC-1 | Access Control Policy | Active"))
			Expect(items[0].Class).To(Equal(oscal.ClassRequirement))

			Expect(items[1].ID).To(Equal("AC-2"))
			Expect(items[1].Text).To(Equal("AC-2 | Account Management | Active"))
			Expect(items[1].Class).To(Equal(oscal.ClassRequirement))
		})

		It("auto-generates ID when first column is empty or too long", func() {
			doc := oscal.StructuredDoc{
				Tables: []oscal.DocTable{
					{
						Headers: []string{"Description", "Notes"},
						Rows: [][]string{
							{"", "Empty ID row"},
							{"This is a very long first column that exceeds fifty characters limit", "Long ID row"},
						},
					},
				},
			}
			items, ok := oscal.TierTable(doc, opts)
			Expect(ok).To(BeTrue())
			Expect(items).To(HaveLen(2))
			Expect(items[0].ID).To(Equal("tbl-1"))
			Expect(items[1].ID).To(Equal("tbl-2"))
		})

		It("handles multiple tables", func() {
			doc := oscal.StructuredDoc{
				Tables: []oscal.DocTable{
					{
						Headers: []string{"ID", "Desc"},
						Rows:    [][]string{{"A-1", "First table"}},
					},
					{
						Headers: []string{"ID", "Desc"},
						Rows: [][]string{
							{"B-1", "Second table"},
							{"B-2", "Second table row 2"},
						},
					},
				},
			}
			items, ok := oscal.TierTable(doc, opts)
			Expect(ok).To(BeTrue())
			Expect(items).To(HaveLen(3))
			Expect(items[0].ID).To(Equal("A-1"))
			Expect(items[1].ID).To(Equal("B-1"))
			Expect(items[2].ID).To(Equal("B-2"))
		})

		It("skips empty rows", func() {
			doc := oscal.StructuredDoc{
				Tables: []oscal.DocTable{
					{
						Headers: []string{"ID", "Desc"},
						Rows: [][]string{
							{},
							{"A-1", "Valid row"},
							{},
						},
					},
				},
			}
			items, ok := oscal.TierTable(doc, opts)
			Expect(ok).To(BeTrue())
			Expect(items).To(HaveLen(1))
			Expect(items[0].ID).To(Equal("A-1"))
		})
	})
})

var _ = Describe("TierLLMDetect", func() {
	Context("when completer is nil", func() {
		It("returns false and nil items", func() {
			doc := oscal.StructuredDoc{RawText: "some text"}
			opts := oscal.StructureOptions{}
			items, ok := oscal.TierLLMDetect(doc, opts, nil, nil)
			Expect(ok).To(BeFalse())
			Expect(items).To(BeNil())
		})
	})

	Context("when SkipLLM is true", func() {
		It("returns false and nil items", func() {
			doc := oscal.StructuredDoc{RawText: "some text"}
			opts := oscal.StructureOptions{SkipLLM: true}
			completer := &testCompleter{response: `\d+\.\s+(.+)`}
			items, ok := oscal.TierLLMDetect(doc, opts, completer, nil)
			Expect(ok).To(BeFalse())
			Expect(items).To(BeNil())
		})
	})

	Context("when the LLM returns a valid pattern", func() {
		It("detects items using the returned regex", func() {
			doc := oscal.StructuredDoc{
				RawText: "1. First requirement\n2. Second requirement\n3. Third requirement",
			}
			opts := oscal.StructureOptions{ChunkChars: 1000}
			completer := &testCompleter{response: `(\d+)\.\s+(.+)`}
			items, ok := oscal.TierLLMDetect(doc, opts, completer, nil)
			Expect(ok).To(BeTrue())
			Expect(items).To(HaveLen(3))
			Expect(items[0].ID).To(Equal("1"))
		})
	})

	Context("when the LLM returns 'none'", func() {
		It("returns false and nil items", func() {
			doc := oscal.StructuredDoc{RawText: "unstructured text"}
			opts := oscal.StructureOptions{}
			completer := &testCompleter{response: "none"}
			items, ok := oscal.TierLLMDetect(doc, opts, completer, nil)
			Expect(ok).To(BeFalse())
			Expect(items).To(BeNil())
		})
	})

	Context("when the completer returns an error", func() {
		It("returns false and nil items", func() {
			completer := &testCompleter{err: errors.New("LLM unavailable")}
			doc := oscal.StructuredDoc{RawText: "1.1 First section\n1.2 Second section\n"}
			opts := oscal.StructureOptions{}
			items, ok := oscal.TierLLMDetect(doc, opts, completer, nil)
			Expect(ok).To(BeFalse())
			Expect(items).To(BeNil())
		})
	})

	Context("with a custom prompt registry", func() {
		It("uses the registry prompt and detects items", func() {
			doc := oscal.StructuredDoc{
				RawText: "Section A.1: First\nSection A.2: Second",
			}
			opts := oscal.StructureOptions{}
			completer := &testCompleter{response: `Section ([A-Z]\.\d+):\s+(.+)`}
			reg := testRegistry(map[string]string{
				"section-detect": "Custom prompt for section detection",
			})
			items, ok := oscal.TierLLMDetect(doc, opts, completer, reg)
			Expect(ok).To(BeTrue())
			Expect(items).To(HaveLen(2))
		})
	})

	Context("with a failing registry (render returns error)", func() {
		It("falls back to hardcoded prompt and still works", func() {
			doc := oscal.StructuredDoc{
				RawText: "1. First\n2. Second\n3. Third",
			}
			opts := oscal.StructureOptions{ChunkChars: 1000}
			completer := &testCompleter{response: `(\d+)\.\s+(.+)`}
			failReg := &failingRegistry{}
			items, ok := oscal.TierLLMDetect(doc, opts, completer, failReg)
			Expect(ok).To(BeTrue())
			Expect(items).To(HaveLen(3))
		})
	})
})

var _ = Describe("TierLLMExtract", func() {
	Context("when completer is nil", func() {
		It("returns false and nil items", func() {
			doc := oscal.StructuredDoc{RawText: "some text"}
			opts := oscal.StructureOptions{}
			items, ok := oscal.TierLLMExtract(doc, opts, nil, nil)
			Expect(ok).To(BeFalse())
			Expect(items).To(BeNil())
		})
	})

	Context("when SkipLLM is true", func() {
		It("returns false and nil items", func() {
			doc := oscal.StructuredDoc{RawText: "some text"}
			opts := oscal.StructureOptions{SkipLLM: true}
			completer := &testCompleter{response: `[]`}
			items, ok := oscal.TierLLMExtract(doc, opts, completer, nil)
			Expect(ok).To(BeFalse())
			Expect(items).To(BeNil())
		})
	})

	Context("when the LLM returns valid JSON", func() {
		It("extracts items with correct fields and Class", func() {
			doc := oscal.StructuredDoc{RawText: "Text with requirements"}
			opts := oscal.StructureOptions{ChunkChars: 1000}
			jsonResponse := `[
				{"id": "req-1", "title": "First", "text": "First requirement text"},
				{"id": "req-2", "title": "Second", "text": "Second requirement text"}
			]`
			completer := &testCompleter{response: jsonResponse}
			items, ok := oscal.TierLLMExtract(doc, opts, completer, nil)
			Expect(ok).To(BeTrue())
			Expect(items).To(HaveLen(2))
			Expect(items[0].ID).To(Equal("req-1"))
			Expect(items[0].Title).To(Equal("First"))
			Expect(items[0].Text).To(Equal("First requirement text"))
			Expect(items[0].Class).To(Equal(oscal.ClassRequirement))
		})
	})

	Context("when the LLM returns invalid JSON", func() {
		It("returns false and nil items", func() {
			doc := oscal.StructuredDoc{RawText: "Text"}
			opts := oscal.StructureOptions{}
			completer := &testCompleter{response: "not json"}
			items, ok := oscal.TierLLMExtract(doc, opts, completer, nil)
			Expect(ok).To(BeFalse())
			Expect(items).To(BeNil())
		})
	})

	Context("when the completer returns an error for all chunks", func() {
		It("returns false and nil items", func() {
			completer := &testCompleter{err: errors.New("LLM unavailable")}
			doc := oscal.StructuredDoc{RawText: "some text that is chunked"}
			opts := oscal.StructureOptions{ChunkChars: 10}
			items, ok := oscal.TierLLMExtract(doc, opts, completer, nil)
			Expect(ok).To(BeFalse())
			Expect(items).To(BeNil())
		})
	})

	Context("when processing multiple chunks", func() {
		It("aggregates items from all chunks", func() {
			longText := strings.Repeat("requirement text\n", 500)
			doc := oscal.StructuredDoc{RawText: longText}
			opts := oscal.StructureOptions{ChunkChars: 1000}
			completer := &testCompleter{
				response: `[{"id": "req-1", "title": "First", "text": "First req"}]`,
			}
			items, ok := oscal.TierLLMExtract(doc, opts, completer, nil)
			Expect(ok).To(BeTrue())
			Expect(len(items)).To(BeNumerically(">=", 2))
		})
	})

	Context("when a single chunk returns more than maxItemsPerChunk items", func() {
		It("caps items at maxItemsPerChunk", func() {
			type item struct {
				ID    string `json:"id"`
				Title string `json:"title"`
				Text  string `json:"text"`
			}
			bigResponse := make([]item, 600)
			for i := range bigResponse {
				bigResponse[i] = item{
					ID:    fmt.Sprintf("item-%d", i),
					Title: fmt.Sprintf("Title %d", i),
					Text:  fmt.Sprintf("Text %d", i),
				}
			}
			responseJSON, err := json.Marshal(bigResponse)
			Expect(err).NotTo(HaveOccurred())

			completer := &testCompleter{response: string(responseJSON)}
			doc := oscal.StructuredDoc{RawText: "some text"}
			opts := oscal.StructureOptions{ChunkChars: 50000}
			items, ok := oscal.TierLLMExtract(doc, opts, completer, nil)
			Expect(ok).To(BeTrue())
			Expect(len(items)).To(BeNumerically("<=", oscal.ExportMaxItemsPerChunk))
		})
	})

	Context("with a custom prompt registry", func() {
		It("uses the registry prompt and extracts items", func() {
			doc := oscal.StructuredDoc{RawText: "Text"}
			opts := oscal.StructureOptions{}
			completer := &testCompleter{
				response: `[{"id": "c-1", "title": "Custom", "text": "Custom req"}]`,
			}
			reg := testRegistry(map[string]string{
				"structured-extract": "Custom extraction prompt",
			})
			items, ok := oscal.TierLLMExtract(doc, opts, completer, reg)
			Expect(ok).To(BeTrue())
			Expect(items).To(HaveLen(1))
		})
	})

	Context("with a failing registry (render returns error)", func() {
		It("falls back to hardcoded prompt and still extracts", func() {
			doc := oscal.StructuredDoc{RawText: "Text with requirements"}
			opts := oscal.StructureOptions{ChunkChars: 1000}
			completer := &testCompleter{
				response: `[{"id": "r-1", "title": "Fallback", "text": "Fallback req"}]`,
			}
			failReg := &failingRegistry{}
			items, ok := oscal.TierLLMExtract(doc, opts, completer, failReg)
			Expect(ok).To(BeTrue())
			Expect(items).To(HaveLen(1))
			Expect(items[0].ID).To(Equal("r-1"))
		})
	})
})

var _ = Describe("ChunkText", func() {
	It("returns a single chunk when text fits", func() {
		text := "Short text"
		chunks := oscal.ChunkText(text, 100)
		Expect(chunks).To(HaveLen(1))
		Expect(chunks[0]).To(Equal(text))
	})

	It("splits into multiple chunks when text exceeds maxChars", func() {
		lines := []string{
			"Line 1: " + strings.Repeat("a", 40),
			"Line 2: " + strings.Repeat("b", 40),
			"Line 3: " + strings.Repeat("c", 40),
			"Line 4: " + strings.Repeat("d", 40),
		}
		text := strings.Join(lines, "\n")
		chunks := oscal.ChunkText(text, 100)
		Expect(len(chunks)).To(BeNumerically(">=", 2))

		for i, chunk := range chunks {
			Expect(len(chunk)).To(BeNumerically("<=", 150), "chunk %d is too large: %d chars", i, len(chunk))
		}

		reconstructed := strings.Join(chunks, "\n")
		Expect(reconstructed).To(Equal(text))
	})

	It("respects newline boundaries", func() {
		text := "Line 1\nLine 2\nLine 3"
		chunks := oscal.ChunkText(text, 10)
		for i, chunk := range chunks {
			if i > 0 {
				Expect(chunk).NotTo(HavePrefix("\n"), "chunk %d starts with newline", i)
			}
		}
	})

	It("uses default chunk size when maxChars is zero", func() {
		lines := make([]string, 100)
		for i := range lines {
			lines[i] = strings.Repeat("x", 50)
		}
		text := strings.Join(lines, "\n")
		chunks := oscal.ChunkText(text, 0)
		Expect(len(chunks)).To(BeNumerically(">=", 2))
	})

	It("handles a single line longer than maxChars", func() {
		text := strings.Repeat("x", 500)
		chunks := oscal.ChunkText(text, 100)
		Expect(chunks).To(HaveLen(1))
		Expect(chunks[0]).To(Equal(text))
	})
})

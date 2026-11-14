package oscal_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	oscalTypes "github.com/defenseunicorns/go-oscal/src/types/oscal-1-1-3"

	"github.com/complytime-labs/crosscodex/pkg/oscal"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
)

// parseFixtureTreeWith parses testdata/tree/<name> with the given parser options.
func parseFixtureTreeWith(name string, opts ...oscal.ParserOption) *oscal.Tree {
	GinkgoHelper()
	f, err := os.Open(filepath.Join("testdata", "tree", name))
	Expect(err).NotTo(HaveOccurred())
	defer func() { Expect(f.Close()).To(Succeed()) }()
	tree, err := oscal.NewParser("", opts...).Parse(context.Background(), f)
	Expect(err).NotTo(HaveOccurred())
	return tree
}

// textlessIDs lists the identifiers of a tree's textless controls in order.
func textlessIDs(tree *oscal.Tree) []string {
	ids := make([]string, 0, len(tree.Textless))
	for _, t := range tree.Textless {
		ids = append(ids, t.Identifier)
	}
	return ids
}

// leafPart is a part without children.
func leafPart(id, name, prose string) oscalTypes.Part {
	return oscalTypes.Part{ID: id, Name: name, Prose: prose}
}

// wrapperPart is a part holding children.
func wrapperPart(id, name, prose string, children ...oscalTypes.Part) oscalTypes.Part {
	return oscalTypes.Part{ID: id, Name: name, Prose: prose, Parts: &children}
}

// controlWithParts is a control with the given title and direct parts.
func controlWithParts(id, title string, parts ...oscalTypes.Part) oscalTypes.Control {
	return oscalTypes.Control{ID: id, Title: title, Parts: &parts}
}

var _ = Describe("Requirement part", func() {
	decompose := func(ctrl oscalTypes.Control, names ...string) map[string]oscal.ControlItem {
		if len(names) == 0 {
			names = oscal.DefaultRequirementPartNames
		}
		byID := map[string]oscal.ControlItem{}
		for _, it := range oscal.DecomposeStatements(ctrl, "g", nil, names) {
			byID[it.ID] = it
		}
		return byID
	}

	Describe("walk to any depth", func() {
		deep := controlWithParts("x-1", "Deep",
			wrapperPart("x-1_smt", "statement", "The system:",
				wrapperPart("x-1_smt.a", "item", "Outer:",
					wrapperPart("x-1_smt.a.1", "item", "Inner:",
						oscalTypes.Part{ID: "x-1_smt.a.1.i", Name: "item", Prose: "Leaf requirement.",
							Props: &[]oscalTypes.Property{{Name: "label", Value: "i."}, {Name: "sort-id", Value: "x-01.a.1.i"}}},
					),
				),
			),
		)

		It("emits the rows in document order", func() {
			var ids []string
			for _, it := range oscal.DecomposeStatements(deep, "g", nil, oscal.DefaultRequirementPartNames) {
				ids = append(ids, it.ID)
			}
			Expect(ids).To(Equal([]string{"x-1", "x-1.a", "x-1.a.1", "x-1.a.1.i"}))
		})

		DescribeTable("classifies each row by whether its part has children",
			func(id, class, parent, text string) {
				it := decompose(deep)[id]
				Expect(it.Class).To(Equal(class))
				Expect(it.ParentID).To(Equal(parent))
				Expect(it.Text).To(Equal(text))
			},
			Entry("the control is a section holding the statement lead-in", "x-1", oscal.ClassSection, "", "The system:"),
			Entry("the outer wrapper is a section under the control", "x-1.a", oscal.ClassSection, "x-1", "Outer:"),
			Entry("the inner wrapper is a section under the outer one", "x-1.a.1", oscal.ClassSection, "x-1.a", "Inner:"),
			Entry("the leaf is a requirement under the inner wrapper", "x-1.a.1.i", oscal.ClassRequirement, "x-1.a.1", "Leaf requirement."),
		)

		It("records the parent and source part of a deep row", func() {
			props := decompose(deep)["x-1.a.1.i"].Props
			Expect(props).To(HaveKeyWithValue("parent-id", "x-1.a.1"))
			Expect(props).To(HaveKeyWithValue("source-part", "x-1_smt.a.1.i"))
		})

		It("keeps the published label and sort-id of a deep row", func() {
			props := decompose(deep)["x-1.a.1.i"].Props
			Expect(props).To(HaveKeyWithValue(oscal.PropLabel, "i."))
			Expect(props).To(HaveKeyWithValue(oscal.PropSortID, "x-01.a.1.i"))
		})

		It("reads a child part whatever its name", func() {
			ctrl := controlWithParts("x-2", "Objective",
				wrapperPart("x-2_smt", "statement", "Determine if:",
					leafPart("x-2_smt.a", "objective", "Accounts are reviewed."),
				),
			)
			it := decompose(ctrl)["x-2.a"]
			Expect(it.Class).To(Equal(oscal.ClassRequirement))
			Expect(it.Text).To(Equal("Accounts are reviewed."))
		})

		It("keeps a single row when the requirement part has no child parts", func() {
			ctrl := controlWithParts("x-3", "Flat", leafPart("x-3_smt", "statement", "Do the thing."))
			result := oscal.DecomposeStatements(ctrl, "g", nil, oscal.DefaultRequirementPartNames)
			Expect(result).To(HaveLen(1))
			Expect(result[0].Class).To(Equal(oscal.ClassRequirement))
			Expect(result[0].Text).To(Equal("Do the thing."))
		})
	})

	Describe("choosing the requirement part", func() {
		It("uses the second same-named part when only it has prose", func() {
			ctrl := controlWithParts("y-1", "Twice",
				leafPart("y-1_smt1", "statement", "  "),
				leafPart("y-1_smt2", "statement", "Second statement."),
			)
			Expect(decompose(ctrl)["y-1"].Text).To(Equal("Second statement."))
		})

		It("uses the first same-named part when both have prose", func() {
			ctrl := controlWithParts("y-2", "Twice",
				leafPart("y-2_smt1", "statement", "First statement."),
				leafPart("y-2_smt2", "statement", "Second statement."),
			)
			Expect(decompose(ctrl)["y-2"].Text).To(Equal("First statement."))
		})

		It("counts prose anywhere in a part's subtree", func() {
			ctrl := controlWithParts("y-3", "Nested",
				wrapperPart("y-3_smt", "statement", "", leafPart("y-3_smt.a", "item", "Nested prose.")),
			)
			Expect(decompose(ctrl)).To(HaveKey("y-3.a"))
		})

		It("tries the names in order", func() {
			ctrl := controlWithParts("y-4", "Ordered",
				leafPart("y-4_smt", "statement", "Statement prose."),
				leafPart("y-4_ovw", "overview", "Overview prose."),
			)
			Expect(decompose(ctrl, "overview", "statement")["y-4"].Text).To(Equal("Overview prose."))
		})

		It("never reads guidance under the default names", func() {
			ctrl := controlWithParts("y-5", "Guided", leafPart("y-5_gdn", "guidance", "Advice only."))
			Expect(decompose(ctrl)["y-5"].Text).To(BeEmpty())
		})
	})

	Describe("AI RMF MANAGE_4.2, whose prose is only in its overview", func() {
		It("has no text under the default names", func() {
			Expect(parseFixture("ai-rmf.json")["MANAGE_4.2"].Text).To(BeEmpty())
		})

		It("takes the overview prose when overview follows statement in the names", func() {
			tree := parseFixtureTreeWith("ai-rmf.json", oscal.WithRequirementPartNames([]string{"statement", "overview"}))
			Expect(itemsByID(tree)["MANAGE_4.2"].Text).To(HavePrefix("Measurable activities for continual improvements"))
		})

		It("still decomposes MANAGE_4.1 by its statement when statement comes first", func() {
			tree := parseFixtureTreeWith("ai-rmf.json", oscal.WithRequirementPartNames([]string{"statement", "overview"}))
			Expect(itemsByID(tree)).To(HaveKey("MANAGE_4.1.1"))
		})

		It("is reported textless because its title only repeats its identifier", func() {
			tree := parseFixtureTree("ai-rmf.json")
			Expect(textlessIDs(tree)).To(ContainElement("MANAGE_4.2"))
			for _, tc := range tree.Textless {
				if tc.Identifier == "MANAGE_4.2" {
					Expect(tc.PartNames).To(ContainElement("overview"))
				}
			}
		})
	})

	DescribeTable("title-only controls have blank text, a usable title and are not textless",
		func(fixture, id string) {
			tree := parseFixtureTree(fixture)
			it := itemsByID(tree)[id]
			Expect(it.Text).To(BeEmpty())
			Expect(oscal.UsableTitle(it.Title, it.ID, it.Props[oscal.PropLabel])).NotTo(BeEmpty())
			Expect(tree.Textless).To(BeEmpty())
		},
		Entry("CIS RHEL 9 leaf rule", "cis-rhel9.json", "cis_rhel9_1-1.1.1"),
		Entry("CIS RHEL 9 wrapper control", "cis-rhel9.json", "cis_rhel9_1-1"),
		Entry("PCI DSS 4 leaf without a statement", "pcidss-4.json", "pcidss_4_2-1.1"),
	)

	Describe("textless report", func() {
		It("reports a control whose only prose is guidance with that part name", func() {
			tree := parseJSONTree(inlineCatalog(`"controls":[` +
				`{"id":"g-1","title":"","parts":[{"id":"g-1_gdn","name":"guidance","prose":"Advice."}]}]`))
			Expect(itemsByID(tree)["g-1"].Text).To(BeEmpty())
			Expect(tree.Textless).To(Equal([]oscal.TextlessControl{{Identifier: "g-1", PartNames: []string{"guidance"}}}))
		})

		It("reports a control with no text whose title equals its ID", func() {
			tree := parseJSONTree(inlineCatalog(`"controls":[{"id":"t-1","title":"T-1"}]`))
			Expect(tree.Textless).To(Equal([]oscal.TextlessControl{{Identifier: "t-1"}}))
		})

		It("reports a control with no text whose title equals its label", func() {
			tree := parseJSONTree(inlineCatalog(`"controls":[` +
				`{"id":"t-2","title":"2.1","props":[{"name":"label","value":"2.1"}]}]`))
			Expect(textlessIDs(tree)).To(Equal([]string{"t-2"}))
		})

		It("lists part names with prose in their subtree, sorted and deduplicated", func() {
			tree := parseJSONTree(inlineCatalog(`"controls":[{"id":"m-1","title":"m-1","parts":[` +
				`{"id":"m-1_ovw","name":"overview","prose":"Overview."},` +
				`{"id":"m-1_smt","name":"statement"},` +
				`{"id":"m-1_g1","name":"guidance","prose":"One."},` +
				`{"id":"m-1_g2","name":"guidance","prose":"Two."},` +
				`{"id":"m-1_ex","name":"example","parts":[{"id":"m-1_ex.a","name":"item","prose":"Deep."}]},` +
				`{"id":"m-1_blank","name":"assessment","prose":"   "}]}]`))
			Expect(tree.Textless).To(HaveLen(1))
			Expect(tree.Textless[0].PartNames).To(Equal([]string{"example", "guidance", "overview"}))
		})

		It("lists textless controls in document order", func() {
			tree := parseJSONTree(inlineCatalog(`"controls":[` +
				`{"id":"z-2","title":"z-2"},` + inlineControl("ok") + `,{"id":"a-1","title":"a-1"}]`))
			Expect(textlessIDs(tree)).To(Equal([]string{"z-2", "a-1"}))
		})

		It("imports a textless control rather than dropping it", func() {
			tree := parseJSONTree(inlineCatalog(`"controls":[{"id":"t-3","title":"t-3"}]`))
			Expect(itemsByID(tree)).To(HaveKey("t-3"))
		})

		It("does not report a control with requirement text and an unusable title", func() {
			tree := parseJSONTree(inlineCatalog(`"controls":[` +
				`{"id":"s-1","title":"s-1","parts":[{"id":"s-1_smt","name":"statement","prose":"Do it."}]}]`))
			Expect(tree.Textless).To(BeEmpty())
		})

		It("records the textless count on the parse span", func() {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = tp.Shutdown(context.Background()) }()
			parser := oscal.NewParser("", oscal.WithParserTracer(tp.TracerProvider().Tracer("test")))
			_, err = parser.Parse(context.Background(), strings.NewReader(inlineCatalog(`"controls":[`+
				`{"id":"t-4","title":"t-4"},{"id":"t-5","title":"t-5"}]`)))
			Expect(err).NotTo(HaveOccurred())
			span := telemetrytest.FindSpan(tp.GetSpans(), "oscal.Parse")
			Expect(span).NotTo(BeNil())
			v, ok := telemetrytest.SpanAttribute(span, "control.textless")
			Expect(ok).To(BeTrue())
			Expect(v.AsInt64()).To(Equal(int64(2)))
		})
	})

	Describe("parser option", func() {
		It("copies the names so later changes to the caller's slice have no effect", func() {
			names := []string{"overview"}
			parser := oscal.NewParser("", oscal.WithRequirementPartNames(names))
			names[0] = "statement"
			tree, err := parser.Parse(context.Background(), strings.NewReader(inlineCatalog(`"controls":[`+
				`{"id":"o-1","title":"O","parts":[{"id":"o-1_smt","name":"statement","prose":"S."},`+
				`{"id":"o-1_ovw","name":"overview","prose":"Overview."}]}]`)))
			Expect(err).NotTo(HaveOccurred())
			Expect(itemsByID(tree)["o-1"].Text).To(Equal("Overview."))
		})

		It("defaults to the statement part", func() {
			Expect(oscal.DefaultRequirementPartNames).To(Equal([]string{"statement"}))
		})
	})

	Describe("item identity", func() {
		It("names both items and the fix when a positional part ID collides with an enhancement", func() {
			doc := inlineCatalog(`"controls":[{"id":"ac-2","title":"Account Management","parts":[` +
				`{"id":"ac-2_smt","name":"statement","prose":"The organization:","parts":[{"name":"item","prose":"Reviews accounts."}]}],` +
				`"controls":[` + inlineControl("ac-2.1") + `]}]`)
			tree, err := oscal.NewParser("").Parse(context.Background(), strings.NewReader(doc))
			Expect(err).To(MatchError(oscal.ErrInvalidFormat))
			Expect(tree).To(BeNil())
			Expect(err.Error()).To(ContainSubstring(`duplicate control ID "ac-2.1"`))
			Expect(err.Error()).To(ContainSubstring(`control "ac-2.1"`))
			Expect(err.Error()).To(ContainSubstring(`numbered by position from a part of control "ac-2"`))
			Expect(err.Error()).To(ContainSubstring("give that part an id"))
		})

		DescribeTable("rejects a catalog whose items cannot be told apart",
			func(controls, wantMsg string) {
				_, err := oscal.NewParser("").Parse(context.Background(), strings.NewReader(inlineCatalog(`"controls":[`+controls+`]`)))
				Expect(err).To(MatchError(oscal.ErrInvalidFormat))
				Expect(err.Error()).To(ContainSubstring(wantMsg))
			},
			Entry("two controls share an ID",
				inlineControl("d-1")+","+inlineControl("d-1"), `duplicate control ID "d-1"`),
			Entry("a statement part derives the ID of a later control",
				`{"id":"d-2","title":"D","parts":[{"id":"d-2_smt","name":"statement","prose":"L:",`+
					`"parts":[{"id":"d-2_smt.a","name":"item","prose":"A."}]}]},`+inlineControl("d-2.a"),
				`duplicate control ID "d-2.a"`),
			Entry("a control has no ID",
				`{"id":"","title":"Untitled","parts":[{"name":"statement","prose":"P."}]}`, `control titled "Untitled" has no id`),
		)
	})
})

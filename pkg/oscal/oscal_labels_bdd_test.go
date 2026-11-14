package oscal_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	oscalTypes "github.com/defenseunicorns/go-oscal/src/types/oscal-1-1-3"

	"github.com/complytime-labs/crosscodex/pkg/oscal"
)

var _ = Describe("Published labels and sort-ids", func() {
	Describe("import", func() {
		It("takes the class-less label and the sort-id of a control", func() {
			props := parseFixture("sp800-53r5.json")["ac-2"].Props
			Expect(props).To(HaveKeyWithValue(oscal.PropLabel, "AC-2"))
			Expect(props).To(HaveKeyWithValue(oscal.PropSortID, "ac-02"))
		})

		It("takes the label of a statement part", func() {
			Expect(parseFixture("sp800-53r5.json")["ac-2.d.1"].Props).To(HaveKeyWithValue(oscal.PropLabel, "1."))
		})

		It("takes the label of a group", func() {
			var label string
			for _, c := range parseFixtureTree("sp800-53r5.json").Containers {
				if c.Identifier == "ac" {
					label = c.Label
				}
			}
			Expect(label).To(Equal("AC"))
		})

		It("takes the published label of an SP 800-171r3 control", func() {
			Expect(parseFixture("sp800-171r3.json")["SP_800_171_03.01.01"].Props).
				To(HaveKeyWithValue(oscal.PropLabel, "Account Management (03.01.01)"))
		})

		It("leaves label and sort-id absent when the catalog publishes none", func() {
			items := parseJSON(inlineCatalog(`"controls":[` + inlineControl("c1") + `]`))
			Expect(items["c1"].Props).NotTo(HaveKey(oscal.PropLabel))
			Expect(items["c1"].Props).NotTo(HaveKey(oscal.PropSortID))
		})
	})

	DescribeTable("selectProp",
		func(props []oscalTypes.Property, wantValue string, wantOK bool) {
			value, ok := oscal.ExportSelectProp(&props, oscal.PropLabel)
			Expect(ok).To(Equal(wantOK))
			Expect(value).To(Equal(wantValue))
		},
		Entry("ignores a label in a foreign namespace",
			[]oscalTypes.Property{{Name: "label", Value: "X", Ns: "http://example.com/ns"}}, "", false),
		Entry("accepts a label in the NIST namespace",
			[]oscalTypes.Property{{Name: "label", Value: "X", Ns: oscal.ExportNISTNamespace}}, "X", true),
		Entry("prefers the class-less label over earlier classed ones",
			[]oscalTypes.Property{
				{Name: "label", Value: "AC-02", Class: "zero-padded"},
				{Name: "label", Value: "AC-2"},
				{Name: "label", Value: "AC-02", Class: "sp800-53a"},
			}, "AC-2", true),
		Entry("takes the first label when all are classed",
			[]oscalTypes.Property{
				{Name: "label", Value: "first", Class: "a"},
				{Name: "label", Value: "second", Class: "b"},
			}, "first", true),
		Entry("ignores a blank value",
			[]oscalTypes.Property{{Name: "label", Value: "  "}}, "", false),
		Entry("ignores props with another name",
			[]oscalTypes.Property{{Name: "sort-id", Value: "x"}}, "", false),
		Entry("reports absence when there are no props", []oscalTypes.Property{}, "", false),
	)

	It("reports absence for a nil prop list", func() {
		value, ok := oscal.ExportSelectProp(nil, oscal.PropLabel)
		Expect(ok).To(BeFalse())
		Expect(value).To(BeEmpty())
	})

	DescribeTable("UsableTitle",
		func(title, identifier, label, want string) {
			Expect(oscal.UsableTitle(title, identifier, label)).To(Equal(want))
		},
		Entry("keeps a real title", "Account Management", "ac-2", "AC-2", "Account Management"),
		Entry("drops a title equal to the label ignoring case", "AC-2", "ac-2", "", ""),
		Entry("drops a title equal to the identifier", "ac-2.1", "ac-2.1", "", ""),
		Entry("drops a title equal to the label", "03.01.01", "x", "03.01.01", ""),
		Entry("drops a blank title", "  ", "a", "", ""),
		Entry("trims a kept title", "  Audit  ", "a", "b", "Audit"),
		Entry("drops a title equal to the identifier but for punctuation and spacing", "MANAGE 4.2", "MANAGE_4.2", "", ""),
		Entry("drops a title equal to the identifier but for case and punctuation", "AC-2", "ac2", "", ""),
		Entry("drops a title without letters or digits", "—", "a", "", ""),
		Entry("keeps a real title that contains the identifier", "MANAGE 4.2 Improvements", "MANAGE_4.2", "", "MANAGE 4.2 Improvements"),
	)
})

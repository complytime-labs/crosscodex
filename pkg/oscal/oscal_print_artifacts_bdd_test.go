package oscal_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/oscal"
)

// Print-artefact patterns are operator data with no built-in default, so the
// specs use two publishers' layouts that an operator might configure.
const (
	// A US GPO page header, one line in Federal Register and CFR PDFs.
	gpoHeader = "VerDate Sep<11>2014 13:52 Jul 11, 2018 Jkt 244001 PO 00000 Frm 00001 Fmt 8010 Sfmt 8010 Y:\\SGML\\244001.XXX 244001"
	// A German page footer.
	deFooter = "Seite 3 von 12"
)

var examplePrintArtifactPatterns = []string{
	`^.*\bVerDate\b.*\bJkt\s+\d+.*\bPO\s+\d{5}.*\bFrm\s+\d+.*\bFmt\s+\d+.*\bSfmt\s+\d+.*$\n?`,
	`^[ \t]*G:\\COMP\\.*$\n?`,
	`^[ \t]*Seite \d+ von \d+[ \t]*$\n?`,
	` ?\[ENTWURF\]`,
}

var _ = Describe("Structurer print-artefact clean-up", func() {
	var structurer oscal.Structurer

	BeforeEach(func() {
		structurer = oscal.NewStructurer(nil, nil)
	})

	structure := func(text string, patterns []string) ([]oscal.ControlItem, error) {
		doc := oscal.StructuredDoc{
			Sections: []oscal.DocSection{{Level: 1, Title: "Requirements", Text: text}},
			RawText:  "Requirements\n\n" + text,
		}
		return structurer.Structure(context.Background(), doc, oscal.StructureOptions{PrintArtifactPatterns: patterns})
	}

	DescribeTable("with patterns for two publishers' layouts",
		func(text, want string) {
			items, err := structure(text, examplePrintArtifactPatterns)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(HaveLen(1))
			Expect(items[0].Text).To(Equal(want))
		},
		Entry("strips a GPO page header line",
			"The agency shall review access.\n"+gpoHeader+"\nThe agency shall log access.",
			"The agency shall review access.\nThe agency shall log access."),
		Entry("strips a G:\\COMP path line",
			"The agency shall review access.\nG:\\COMP\\INTEL\\NSA1947.XML\nThe agency shall log access.",
			"The agency shall review access.\nThe agency shall log access."),
		Entry("strips a German page footer line",
			"Die Organisation muss den Zugang prüfen.\n"+deFooter+"\nDie Organisation muss den Zugang protokollieren.",
			"Die Organisation muss den Zugang prüfen.\nDie Organisation muss den Zugang protokollieren."),
		Entry("strips a span within a line",
			"Die Organisation muss den Zugang prüfen. [ENTWURF]",
			"Die Organisation muss den Zugang prüfen."),
		Entry("keeps a line that only mentions a page",
			"Siehe Seite 3 von 12 im Anhang.",
			"Siehe Seite 3 von 12 im Anhang."),
		Entry("keeps the indentation of the remaining text",
			"    indented requirement shall hold.\n"+gpoHeader,
			"    indented requirement shall hold."),
		Entry("keeps a line citing a PO number",
			"Obtain approval for PO 12345 before release.",
			"Obtain approval for PO 12345 before release."),
		Entry("keeps a line with only some header tokens",
			"Record the Jkt 1234 and Frm 0001 values.",
			"Record the Jkt 1234 and Frm 0001 values."),
	)

	It("keeps a statement the patterns would erase entirely", func() {
		items, err := structure(gpoHeader, examplePrintArtifactPatterns)
		Expect(err).NotTo(HaveOccurred())
		Expect(items).To(HaveLen(1))
		Expect(items[0].Text).To(Equal(gpoHeader))
	})

	It("strips nothing when no patterns are given", func() {
		text := "The agency shall review access.\n" + gpoHeader
		items, err := structure(text, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(items[0].Text).To(Equal(text))
	})

	It("strips from titles as well as text", func() {
		doc := oscal.StructuredDoc{
			Sections: []oscal.DocSection{{Level: 1, Title: "Zugangskontrolle [ENTWURF]", Text: "Die Organisation muss den Zugang prüfen."}},
			RawText:  "Zugangskontrolle [ENTWURF]\n\nDie Organisation muss den Zugang prüfen.",
		}
		items, err := structurer.Structure(context.Background(), doc, oscal.StructureOptions{PrintArtifactPatterns: examplePrintArtifactPatterns})
		Expect(err).NotTo(HaveOccurred())
		Expect(items[0].Title).To(Equal("Zugangskontrolle"))
	})

	DescribeTable("rejects an unusable pattern with an actionable error",
		func(pattern, reason string) {
			items, err := structure("The agency shall review access.", []string{pattern})
			Expect(err).To(MatchError(oscal.ErrInvalidPrintArtifactPattern))
			Expect(err.Error()).To(ContainSubstring(reason))
			Expect(err.Error()).To(ContainSubstring("catalog.structuring.print_artifact_patterns"))
			Expect(items).To(BeNil())
		},
		Entry("invalid regex", "(unclosed", "is not valid regex"),
		Entry("a pattern matching empty text", "x*", "matches empty text"),
	)
})

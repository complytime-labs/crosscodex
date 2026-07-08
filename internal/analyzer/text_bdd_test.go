package analyzer_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/prompt"
)

var _ = Describe("TruncateText", func() {
	It("replaces newlines with spaces", func() {
		Expect(analyzer.TruncateText("line1\nline2\rline3", 100)).To(Equal("line1 line2 line3"))
	})

	It("truncates to maxChars on rune boundary", func() {
		Expect(analyzer.TruncateText("abcdef", 3)).To(Equal("abc"))
	})

	It("handles multi-byte runes correctly", func() {
		Expect(analyzer.TruncateText("αβγδ", 2)).To(Equal("αβ"))
	})

	It("returns full text when under limit", func() {
		Expect(analyzer.TruncateText("short", 100)).To(Equal("short"))
	})

	It("no-ops when maxChars is 0", func() {
		Expect(analyzer.TruncateText("anything", 0)).To(Equal("anything"))
	})
})

var _ = Describe("FormatFewShotExamples", func() {
	It("returns empty string for nil slice", func() {
		Expect(analyzer.FormatFewShotExamples(nil)).To(BeEmpty())
	})

	It("returns empty string for empty slice", func() {
		Expect(analyzer.FormatFewShotExamples([]prompt.FewShotExample{})).To(BeEmpty())
	})

	It("formats numbered examples with header", func() {
		examples := []prompt.FewShotExample{
			{Input: "input1", Output: "output1"},
			{Input: "input2", Output: "output2"},
		}
		result := analyzer.FormatFewShotExamples(examples)
		Expect(result).To(HavePrefix("EXAMPLES:\n\n"))
		Expect(result).To(ContainSubstring("Example 1:\ninput1\nExpected output:\noutput1\n"))
		Expect(result).To(ContainSubstring("Example 2:\ninput2\nExpected output:\noutput2\n"))
	})
})

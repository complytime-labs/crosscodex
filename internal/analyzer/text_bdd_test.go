package analyzer_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/prompt"
)

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

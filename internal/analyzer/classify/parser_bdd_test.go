package classify_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/analyzer/classify"
)

var _ = Describe("ParseClassification", func() {
	expectParsed := func(input string, expectedType classify.ClassificationType, expectedLevel classify.ClassificationLevel) {
		result, err := classify.ParseClassification(input)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Type).To(Equal(expectedType))
		Expect(result.Level).To(Equal(expectedLevel))
	}

	Context("all 16 valid type|level combinations", func() {
		DescribeTable("parses correctly", expectParsed,
			Entry("Technical|Strategic", "Technical|Strategic", classify.TypeTechnical, classify.LevelStrategic),
			Entry("Technical|Tactical", "Technical|Tactical", classify.TypeTechnical, classify.LevelTactical),
			Entry("Technical|Operational", "Technical|Operational", classify.TypeTechnical, classify.LevelOperational),
			Entry("Technical|None", "Technical|None", classify.TypeTechnical, classify.LevelNone),
			Entry("Procedural|Strategic", "Procedural|Strategic", classify.TypeProcedural, classify.LevelStrategic),
			Entry("Procedural|Tactical", "Procedural|Tactical", classify.TypeProcedural, classify.LevelTactical),
			Entry("Procedural|Operational", "Procedural|Operational", classify.TypeProcedural, classify.LevelOperational),
			Entry("Procedural|None", "Procedural|None", classify.TypeProcedural, classify.LevelNone),
			Entry("Both|Strategic", "Both|Strategic", classify.TypeBoth, classify.LevelStrategic),
			Entry("Both|Tactical", "Both|Tactical", classify.TypeBoth, classify.LevelTactical),
			Entry("Both|Operational", "Both|Operational", classify.TypeBoth, classify.LevelOperational),
			Entry("Both|None", "Both|None", classify.TypeBoth, classify.LevelNone),
			Entry("None|None", "None|None", classify.TypeNone, classify.LevelNone),
			Entry("None|Strategic -> None|None", "None|Strategic", classify.TypeNone, classify.LevelNone),
			Entry("None|Tactical -> None|None", "None|Tactical", classify.TypeNone, classify.LevelNone),
			Entry("None|Operational -> None|None", "None|Operational", classify.TypeNone, classify.LevelNone),
		)
	})

	Context("case insensitivity", func() {
		DescribeTable("handles mixed case", expectParsed,
			Entry("all caps", "TECHNICAL|OPERATIONAL", classify.TypeTechnical, classify.LevelOperational),
			Entry("mixed case", "tEcHnIcAl|oPeRaTiOnAl", classify.TypeTechnical, classify.LevelOperational),
			Entry("all lower", "procedural|strategic", classify.TypeProcedural, classify.LevelStrategic),
		)
	})

	DescribeTable("lenient and fail-closed parsing", expectParsed,
		Entry("strips leading/trailing whitespace", "  Technical|Operational  ", classify.TypeTechnical, classify.LevelOperational),
		Entry("strips trailing periods", "Technical|Operational.", classify.TypeTechnical, classify.LevelOperational),
		Entry("strips newlines", "Technical|Operational\n", classify.TypeTechnical, classify.LevelOperational),
		Entry("handles carriage return + newline", "Both|Tactical\r\n", classify.TypeBoth, classify.LevelTactical),
		Entry("missing pipe defaults level to Tactical", "Technical", classify.TypeTechnical, classify.LevelTactical),
		Entry("missing pipe maps None type to None level (overrides Tactical default)", "None", classify.TypeNone, classify.LevelNone),
		Entry("maps 'noise' to None|None", "noise|tactical", classify.TypeNone, classify.LevelNone),
		Entry("maps 'Noise' alone to None|None", "Noise", classify.TypeNone, classify.LevelNone),
		Entry("maps unknown type to None (None type forces None level)", "garbled|Operational", classify.TypeNone, classify.LevelNone),
		Entry("maps unknown level to Tactical", "Technical|garbled", classify.TypeTechnical, classify.LevelTactical),
		Entry("matches 'technical' as substring in longer text", "purely technical|operational", classify.TypeTechnical, classify.LevelOperational),
		Entry("matches with extra pipe segments (takes first two)", "Technical|Operational|extra", classify.TypeTechnical, classify.LevelOperational),
	)

	DescribeTable("empty input returns an error mentioning empty",
		func(input string) {
			_, err := classify.ParseClassification(input)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("empty"))
		},
		Entry("empty string", ""),
		Entry("whitespace-only string", "   \n\t  "),
	)

	Context("Python parity -- all 8 default few-shot outputs", func() {
		DescribeTable("matches Python parse_response", expectParsed,
			Entry("Technical|Operational", "Technical|Operational", classify.TypeTechnical, classify.LevelOperational),
			Entry("Procedural|Tactical", "Procedural|Tactical", classify.TypeProcedural, classify.LevelTactical),
			Entry("Both|Tactical", "Both|Tactical", classify.TypeBoth, classify.LevelTactical),
			Entry("Procedural|Strategic", "Procedural|Strategic", classify.TypeProcedural, classify.LevelStrategic),
			Entry("Both|Strategic", "Both|Strategic", classify.TypeBoth, classify.LevelStrategic),
			Entry("None|None", "None|None", classify.TypeNone, classify.LevelNone),
		)
	})
})

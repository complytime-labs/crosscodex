package classify_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/analyzer/classify"
)

var _ = Describe("ParseClassification", func() {
	Context("all 16 valid type|level combinations", func() {
		DescribeTable("parses correctly",
			func(input string, expectedType classify.ClassificationType, expectedLevel classify.ClassificationLevel) {
				result, err := classify.ParseClassification(input)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.Type).To(Equal(expectedType))
				Expect(result.Level).To(Equal(expectedLevel))
			},
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
		DescribeTable("handles mixed case",
			func(input string, expectedType classify.ClassificationType, expectedLevel classify.ClassificationLevel) {
				result, err := classify.ParseClassification(input)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.Type).To(Equal(expectedType))
				Expect(result.Level).To(Equal(expectedLevel))
			},
			Entry("all caps", "TECHNICAL|OPERATIONAL", classify.TypeTechnical, classify.LevelOperational),
			Entry("mixed case", "tEcHnIcAl|oPeRaTiOnAl", classify.TypeTechnical, classify.LevelOperational),
			Entry("all lower", "procedural|strategic", classify.TypeProcedural, classify.LevelStrategic),
		)
	})

	Context("whitespace and punctuation stripping", func() {
		It("strips leading/trailing whitespace", func() {
			result, err := classify.ParseClassification("  Technical|Operational  ")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Type).To(Equal(classify.TypeTechnical))
			Expect(result.Level).To(Equal(classify.LevelOperational))
		})

		It("strips trailing periods", func() {
			result, err := classify.ParseClassification("Technical|Operational.")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Type).To(Equal(classify.TypeTechnical))
		})

		It("strips newlines", func() {
			result, err := classify.ParseClassification("Technical|Operational\n")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Type).To(Equal(classify.TypeTechnical))
		})

		It("handles carriage return + newline", func() {
			result, err := classify.ParseClassification("Both|Tactical\r\n")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Type).To(Equal(classify.TypeBoth))
			Expect(result.Level).To(Equal(classify.LevelTactical))
		})
	})

	Context("missing pipe (no delimiter)", func() {
		It("defaults level to Tactical", func() {
			result, err := classify.ParseClassification("Technical")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Type).To(Equal(classify.TypeTechnical))
			Expect(result.Level).To(Equal(classify.LevelTactical))
		})

		It("maps None type to None level (overrides Tactical default)", func() {
			result, err := classify.ParseClassification("None")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Type).To(Equal(classify.TypeNone))
			Expect(result.Level).To(Equal(classify.LevelNone))
		})
	})

	Context("noise keyword", func() {
		It("maps 'noise' to None|None", func() {
			result, err := classify.ParseClassification("noise|tactical")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Type).To(Equal(classify.TypeNone))
			Expect(result.Level).To(Equal(classify.LevelNone))
		})

		It("maps 'Noise' alone to None|None", func() {
			result, err := classify.ParseClassification("Noise")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Type).To(Equal(classify.TypeNone))
			Expect(result.Level).To(Equal(classify.LevelNone))
		})
	})

	Context("unknown strings (fail-closed)", func() {
		It("maps unknown type to None", func() {
			result, err := classify.ParseClassification("garbled|Operational")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Type).To(Equal(classify.TypeNone))
			Expect(result.Level).To(Equal(classify.LevelNone)) // None type forces None level
		})

		It("maps unknown level to Tactical", func() {
			result, err := classify.ParseClassification("Technical|garbled")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Type).To(Equal(classify.TypeTechnical))
			Expect(result.Level).To(Equal(classify.LevelTactical))
		})
	})

	Context("empty input", func() {
		It("returns error for empty string", func() {
			_, err := classify.ParseClassification("")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("empty"))
		})

		It("returns error for whitespace-only string", func() {
			_, err := classify.ParseClassification("   \n\t  ")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("empty"))
		})
	})

	Context("Python parity -- all 8 default few-shot outputs", func() {
		DescribeTable("matches Python parse_response",
			func(input string, expectedType classify.ClassificationType, expectedLevel classify.ClassificationLevel) {
				result, err := classify.ParseClassification(input)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.Type).To(Equal(expectedType))
				Expect(result.Level).To(Equal(expectedLevel))
			},
			Entry("Technical|Operational", "Technical|Operational", classify.TypeTechnical, classify.LevelOperational),
			Entry("Procedural|Tactical", "Procedural|Tactical", classify.TypeProcedural, classify.LevelTactical),
			Entry("Both|Tactical", "Both|Tactical", classify.TypeBoth, classify.LevelTactical),
			Entry("Procedural|Strategic", "Procedural|Strategic", classify.TypeProcedural, classify.LevelStrategic),
			Entry("Both|Strategic", "Both|Strategic", classify.TypeBoth, classify.LevelStrategic),
			Entry("None|None", "None|None", classify.TypeNone, classify.LevelNone),
		)
	})

	Context("substring matching edge cases", func() {
		It("matches 'technical' as substring in longer text", func() {
			result, err := classify.ParseClassification("purely technical|operational")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Type).To(Equal(classify.TypeTechnical))
		})

		It("matches with extra pipe segments (takes first two)", func() {
			result, err := classify.ParseClassification("Technical|Operational|extra")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Type).To(Equal(classify.TypeTechnical))
			Expect(result.Level).To(Equal(classify.LevelOperational))
		})
	})
})

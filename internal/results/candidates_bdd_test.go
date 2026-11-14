package results_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/results"
)

var _ = Describe("decodeContributions", func() {
	DescribeTable("accepts well-formed provenance",
		func(raw string, want []results.Contribution) {
			got, err := results.DecodeContributions([]byte(raw))
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(want))
		},
		Entry("two generators with their scores, weights and metadata",
			`[{"GeneratorName":"embedding","Score":0.8,"Weight":0.6,"Metadata":{"rank":"1"}},
			  {"GeneratorName":"keyword","Score":0.25,"Weight":0.4,"Metadata":null}]`,
			[]results.Contribution{
				{Generator: "embedding", Score: 0.8, Weight: 0.6, Metadata: map[string]string{"rank": "1"}},
				{Generator: "keyword", Score: 0.25, Weight: 0.4},
			}),
		Entry("an empty array has no contributions", `[]`, []results.Contribution{}),
		Entry("null has no contributions", `null`, []results.Contribution(nil)),
	)

	DescribeTable("rejects corrupt provenance",
		func(raw string) {
			got, err := results.DecodeContributions([]byte(raw))
			Expect(err).To(MatchError(results.ErrCorruptProvenance))
			Expect(got).To(BeNil())
		},
		Entry("an object instead of an array", `{}`),
		Entry("a missing generator name", `[{"Score":0.5,"Weight":0.5}]`),
		Entry("an empty generator name", `[{"GeneratorName":"","Score":0.5,"Weight":0.5}]`),
		Entry("a null element", `[null]`),
		Entry("a string score", `[{"GeneratorName":"g","Score":"NaN","Weight":0.5}]`),
		Entry("a non-string metadata value", `[{"GeneratorName":"g","Score":0.5,"Weight":0.5,"Metadata":{"rank":1}}]`),
		Entry("an unknown key", `[{"GeneratorName":"g","Score":0.5,"Weight":0.5,"Extra":true}]`),
		Entry("a number that overflows float64", `[{"GeneratorName":"g","Score":1e999,"Weight":0.5}]`),
		Entry("data after the array", `[] []`),
		Entry("truncated JSON", `[{"GeneratorName":"g","Score":0.5`),
	)
})

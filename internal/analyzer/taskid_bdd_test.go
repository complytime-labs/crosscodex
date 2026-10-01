//go:build !integration

package analyzer_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	intanalyzer "github.com/complytime-labs/crosscodex/internal/analyzer"
)

var _ = Describe("StripTaskIDPrefix", func() {
	It("strips a matching prefix and rejects a mismatched one", func() {
		rest, ok := intanalyzer.StripTaskIDPrefix("classify-AC-1", "classify")
		Expect(ok).To(BeTrue())
		Expect(rest).To(Equal("AC-1"))

		_, ok = intanalyzer.StripTaskIDPrefix("other-AC-1", "classify")
		Expect(ok).To(BeFalse(), "expected ok=false for mismatched prefix")
	})
})

var _ = Describe("ParsePairTaskID", func() {
	It("parses a pair task ID", func() {
		models := []string{"llama3.2:3b", "qwen3:8b"}
		source, target, model, sample, ok := intanalyzer.ParsePairTaskID(
			"relationship-AC-1--SC-7(2)-qwen3:8b-s2", "relationship", models)
		Expect(ok).To(BeTrue())
		Expect(source).To(Equal("AC-1"))
		Expect(target).To(Equal("SC-7(2)"))
		Expect(model).To(Equal("qwen3:8b"))
		Expect(sample).To(Equal(2))
	})

	DescribeTable("rejects invalid task IDs",
		func(taskID string) {
			models := []string{"llama3.2:3b", "qwen3:8b"}
			_, _, _, _, ok := intanalyzer.ParsePairTaskID(taskID, "relationship", models)
			Expect(ok).To(BeFalse(), "expected ok=false for %q", taskID)
		},
		Entry("missing prefix", "other-AC-1--AC-2-qwen3:8b-s0"),
		Entry("missing pairwise separator", "relationship-AC-1-AC-2-qwen3:8b-s0"),
		Entry("no sample suffix", "relationship-AC-1--AC-2-qwen3:8b"),
		Entry("model not in configured list", "relationship-AC-1--AC-2-unknown-model-s0"),
	)
})

var _ = Describe("ParseControlTaskID", func() {
	It("parses a control task ID", func() {
		models := []string{"llama3.2:3b"}
		controlID, model, sample, skipped, ok := intanalyzer.ParseControlTaskID(
			"artifacts-AC-1-llama3.2:3b-s0", "artifacts", models)
		Expect(ok).To(BeTrue())
		Expect(skipped).To(BeFalse())
		Expect(controlID).To(Equal("AC-1"))
		Expect(model).To(Equal("llama3.2:3b"))
		Expect(sample).To(Equal(0))

		controlID, _, _, skipped, ok = intanalyzer.ParseControlTaskID(
			"artifacts-AC-1-skip", "artifacts", models)
		Expect(ok).To(BeTrue())
		Expect(skipped).To(BeTrue())
		Expect(controlID).To(Equal("AC-1"))
	})

	DescribeTable("rejects invalid task IDs",
		func(taskID string) {
			models := []string{"llama3.2:3b"}
			_, _, _, _, ok := intanalyzer.ParseControlTaskID(taskID, "artifacts", models)
			Expect(ok).To(BeFalse(), "expected ok=false for %q", taskID)
		},
		Entry("missing prefix", "other-AC-1-llama3.2:3b-s0"),
		Entry("no sample suffix and not a skip", "artifacts-AC-1-llama3.2:3b"),
		Entry("model not in configured list", "artifacts-AC-1-unknown-model-s0"),
	)
})

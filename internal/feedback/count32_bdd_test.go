//go:build !integration

package feedback_test

import (
	"math"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/feedback"
)

var _ = Describe("count32", func() {
	DescribeTable("narrows a count for an int32 field",
		func(n int, want int32) {
			got, err := feedback.Count32("votes", n)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(want))
		},
		Entry("zero", 0, int32(0)),
		Entry("the largest int32", math.MaxInt32, int32(math.MaxInt32)),
	)

	DescribeTable("refuses a count that does not fit, naming the field and returning zero",
		func(n int, wantMsg string) {
			got, err := feedback.Count32("votes", n)
			Expect(err).To(MatchError(wantMsg))
			Expect(got).To(BeZero())
		},
		Entry("one above the largest int32", math.MaxInt32+1, "votes 2147483648 does not fit the response's int32 field"),
		Entry("negative", -1, "votes -1 does not fit the response's int32 field"),
	)
})

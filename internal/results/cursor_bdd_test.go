package results_test

import (
	"encoding/base64"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/results"
)

var _ = Describe("page cursor", func() {
	It("round-trips a cursor through its token", func() {
		c := results.Cursor{JobID: "job-1", SourceID: "cat/a", TargetID: "cat/b"}
		got, err := results.DecodeCursor(results.EncodeCursor(c))
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(&c))
	})
	It("decodes the empty token as the first page", func() {
		got, err := results.DecodeCursor("")
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(BeNil())
	})
	DescribeTable("rejects malformed tokens with an actionable error",
		func(token string) {
			_, err := results.DecodeCursor(token)
			Expect(err).To(MatchError(results.ErrInvalidPageToken))
			Expect(err.Error()).To(ContainSubstring("next_page_token"))
		},
		Entry("not base64", "!!!"),
		Entry("base64 of non-JSON", "bm90LWpzb24"),
		Entry("wrong arity", base64.RawURLEncoding.EncodeToString([]byte(`["a","b"]`))),
		Entry("non-string element", base64.RawURLEncoding.EncodeToString([]byte(`["a","b",3]`))),
		Entry("oversized", strings.Repeat("A", 8193)),
	)
})

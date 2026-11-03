//go:build !integration

package artifacts_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/analyzer/artifacts"
)

var _ = Describe("PairRef", func() {
	It("is the first 16 hex characters of sha256(pair key)", func() {
		key := artifacts.PairKey("g-a", "g-b")
		sum := sha256.Sum256([]byte(key))
		Expect(artifacts.PairRef(key)).To(Equal(hex.EncodeToString(sum[:])[:16]))
	})
	It("differs for different pairs", func() {
		Expect(artifacts.PairRef(artifacts.PairKey("a", "b"))).NotTo(Equal(artifacts.PairRef(artifacts.PairKey("a", "c"))))
	})
	It("passes ValidatePairRef", func() {
		Expect(artifacts.ValidatePairRef(artifacts.PairRef(artifacts.PairKey("a", "b")))).To(Succeed())
	})
})

var _ = Describe("ValidatePairRef", func() {
	DescribeTable("rejects with an actionable message",
		func(ref string) {
			Expect(artifacts.ValidatePairRef(ref)).To(MatchError(ContainSubstring("copy the PAIR column from the review listing")))
		},
		Entry("empty", ""),
		Entry("too short", "abc123"),
		Entry("too long", strings.Repeat("a", 17)),
		Entry("uppercase hex", strings.Repeat("A", 16)),
		Entry("non-hex", strings.Repeat("g", 16)),
		Entry("a pair key fragment", "artifact-pair|a|b"),
	)
})

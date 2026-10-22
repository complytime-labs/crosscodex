//go:build !integration

package db_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/db"
)

var _ = Describe("SCRAMVerifier", func() {
	salt := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}

	It("matches an independently computed RFC 7677 verifier", func() {
		// Computed with Python hashlib.pbkdf2_hmac / hmac.
		v, err := db.SCRAMVerifier("correct-horse-battery-staple", salt)
		Expect(err).NotTo(HaveOccurred())
		Expect(v).To(Equal("SCRAM-SHA-256$4096:AAECAwQFBgcICQoLDA0ODw==$kWNFaTEPbUt9WrpDQi/SVLi++w5hBzrJjZHv9AoFZlM=:thnlKrViH0JqoSdbWrksVs+mYMBgtGd1lhG9C8ym4xQ="))
	})

	DescribeTable("refuses a password it cannot hash the way PostgreSQL does",
		func(password, want string) {
			_, err := db.SCRAMVerifier(password, salt)
			Expect(err).To(MatchError(ContainSubstring(want)))
		},
		Entry("empty", "", "empty"),
		Entry("a space", "has space", "printable ASCII"),
		Entry("non-ASCII", "pässword", "printable ASCII"),
		Entry("a control character", "tab\there", "printable ASCII"),
	)

	It("refuses a salt shorter than 16 bytes", func() {
		_, err := db.SCRAMVerifier("password", []byte{1, 2, 3})
		Expect(err).To(MatchError(ContainSubstring("16 bytes")))
	})
})

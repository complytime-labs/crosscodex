package dbtest_test

import (
	"encoding/hex"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
)

var _ = Describe("Property Specifications", Ordered, func() {
	It("AlterRolePasswordSQL accepts exactly the non-empty hex passwords and never emits a stray quote", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			role := rapid.StringMatching(`[a-z_]{1,20}`).Draw(t, "role")
			password := rapid.OneOf(
				rapid.StringMatching(`([0-9a-f]{2}){1,32}`),
				rapid.String(),
			).Draw(t, "password")
			_, decodeErr := hex.DecodeString(password)
			wantOK := password != "" && decodeErr == nil

			stmt, err := dbtest.AlterRolePasswordSQL(role, password)
			if wantOK != (err == nil) {
				t.Fatalf("password %q: want accepted=%v, got err=%v", password, wantOK, err)
			}
			if err == nil && strings.Count(stmt, "'") != 2 {
				t.Fatalf("statement %q must contain exactly the two quotes around the password", stmt)
			}
		})
	})
})

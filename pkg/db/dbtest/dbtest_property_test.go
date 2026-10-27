package dbtest_test

import (
	"encoding/hex"
	"fmt"
	"net/url"
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

	It("HardDeleteTenantSQL accepts every well-formed tenant ID and quotes exactly that ID", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			id := rapid.StringMatching("^[a-z][a-z0-9-]{1,50}[a-z0-9]$").Draw(t, "id")

			stmt, err := dbtest.HardDeleteTenantSQL(id)
			if err != nil {
				t.Fatalf("well-formed tenant ID %q refused: %v", id, err)
			}
			if !strings.Contains(stmt, "'"+id+"'") {
				t.Fatalf("statement does not quote ID %q", id)
			}
			if got := strings.Count(stmt, "'crosscodex_"+id+"'"); got != 2 {
				t.Fatalf("statement must quote the graph name for %q exactly twice, found %d", id, got)
			}
		})
	})

	It("RoleDSN carries exactly the role and its password and keeps every other part of the URL", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			in := &url.URL{
				Scheme:   rapid.SampledFrom([]string{"postgres", "postgresql"}).Draw(t, "scheme"),
				User:     url.UserPassword(rapid.String().Draw(t, "user"), rapid.String().Draw(t, "suPassword")),
				Host:     fmt.Sprintf("%s:%d", rapid.StringMatching(`[a-z][a-z0-9.-]{0,20}`).Draw(t, "host"), rapid.IntRange(1, 65535).Draw(t, "port")),
				Path:     "/" + rapid.StringMatching(`[a-z_]{1,20}`).Draw(t, "db"),
				RawQuery: url.Values{"sslmode": {rapid.SampledFrom([]string{"disable", "require", "verify-full"}).Draw(t, "sslmode")}}.Encode(),
			}
			role := rapid.StringMatching(`[a-z_]{1,20}`).Draw(t, "role")
			password := rapid.StringMatching(`([0-9a-f]{2}){1,32}`).Draw(t, "password")

			dsn, err := dbtest.RoleDSN(in.String(), role, password)
			if err != nil {
				t.Fatalf("RoleDSN(%q) refused a postgres URL: %v", in.Redacted(), err)
			}
			out, err := url.Parse(dsn)
			if err != nil {
				t.Fatalf("RoleDSN returned an unparsable DSN: %v", err)
			}
			if got, _ := out.User.Password(); out.User.Username() != role || got != password {
				t.Fatalf("userinfo is %q, want role %q with the given password", out.User.Username(), role)
			}
			if out.Scheme != in.Scheme || out.Host != in.Host || out.Path != in.Path || out.RawQuery != in.RawQuery {
				t.Fatalf("RoleDSN changed more than the userinfo: %s -> %s", in.Redacted(), out.Redacted())
			}
		})
	})
})

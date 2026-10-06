package dbtest_test

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
)

func FuzzAlterRolePasswordSQL(f *testing.F) {
	f.Add("app_user", "0123abcd")           // valid
	f.Add("app_user", "")                   // empty
	f.Add("app_user", "abc")                // odd-length hex
	f.Add("purge_user", "ab'cd")            // quote
	f.Add(`we"ird`, "00ff")                 // role needing quoting
	f.Add("app_user", "x'; DROP ROLE a --") // SQL breakout

	f.Fuzz(func(t *testing.T, role, password string) {
		stmt, err := dbtest.AlterRolePasswordSQL(role, password)
		if err != nil {
			return
		}
		if strings.ContainsAny(password, "'\\;") {
			t.Fatalf("accepted unsafe password %q", password)
		}
		quotedRole := pgx.Identifier{role}.Sanitize()
		if !strings.HasPrefix(stmt, "ALTER ROLE "+quotedRole+" ") {
			t.Fatalf("statement %q does not start with the quoted role %s", stmt, quotedRole)
		}
		if !strings.HasSuffix(stmt, " WITH PASSWORD '"+password+"'") {
			t.Fatalf("statement %q does not end with the quoted password", stmt)
		}
	})
}

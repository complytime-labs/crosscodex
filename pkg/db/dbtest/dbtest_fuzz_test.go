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

func FuzzHardDeleteTenantSQL(f *testing.F) {
	f.Add("acme")                  // valid
	f.Add("")                      // empty
	f.Add("a'b")                   // quote
	f.Add(strings.Repeat("a", 60)) // too long
	f.Add("a'; DROP TABLE tenants; --")

	f.Fuzz(func(t *testing.T, id string) {
		stmt, err := dbtest.HardDeleteTenantSQL(id)
		if err != nil {
			return
		}
		if strings.Trim(id, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" {
			t.Fatalf("accepted tenant ID %q outside [a-z0-9-]", id)
		}
		if !strings.Contains(stmt, "'"+id+"'") {
			t.Fatalf("statement does not quote the accepted ID %q", id)
		}
	})
}

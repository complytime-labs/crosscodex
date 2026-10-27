package dbtest_test

import (
	"encoding/hex"
	"net/url"
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

func FuzzRoleDSN(f *testing.F) {
	const pw = "0123456789abcdef"
	f.Add("postgres://postgres:secret@db:5432/crosscodex_test?sslmode=verify-full", pw) // valid
	f.Add("postgresql:///crosscodex?host=/run/postgresql", pw)                          // valid, no host
	f.Add("", pw)                                                                       // empty DSN
	f.Add("postgres://su:secret@db:5432/x", "")                                         // empty password
	f.Add("postgres://su:secret@db:5432/x", "ab@cd")                                    // userinfo-smuggling password
	f.Add("host=db user=su password=secret", pw)                                        // keyword/value DSN
	f.Add("postgres:secret@db/x", pw)                                                   // opaque
	f.Add("postgres://su:secret@db:port/x", pw)                                         // invalid port
	f.Add("postgres://evil@db/x?user=postgres#frag", pw)                                // userinfo smuggled in the query

	f.Fuzz(func(t *testing.T, dsn, password string) {
		out, err := dbtest.RoleDSN(dsn, "app_user", password)
		if err != nil {
			return
		}
		if _, decodeErr := hex.DecodeString(password); password == "" || decodeErr != nil {
			t.Fatalf("accepted non-hex password %q", password)
		}
		in, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("accepted unparsable DSN %q", dsn)
		}
		u, err := url.Parse(out)
		if err != nil {
			t.Fatalf("returned unparsable DSN %q for %q: %v", out, dsn, err)
		}
		if u.Scheme != in.Scheme || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
			t.Fatalf("scheme %q from input scheme %q", u.Scheme, in.Scheme)
		}
		if got, _ := u.User.Password(); u.User.Username() != "app_user" || got != password {
			t.Fatalf("output %q does not authenticate as app_user with the given password", out)
		}
		if u.Host != in.Host || u.Path != in.Path || u.RawQuery != in.RawQuery {
			t.Fatalf("RoleDSN changed more than the userinfo: %q -> %q", dsn, out)
		}
	})
}

package dbtest_test

import (
	"context"
	"net/url"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
)

func TestDBTest(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "DBTest Suite")
}

var _ = Describe("NewRolePassword", func() {
	It("returns 32 lowercase hex characters, fresh on every call", func() {
		a, err := dbtest.NewRolePassword()
		Expect(err).NotTo(HaveOccurred())
		b, err := dbtest.NewRolePassword()
		Expect(err).NotTo(HaveOccurred())
		Expect(a).To(MatchRegexp(`^[0-9a-f]{32}$`))
		Expect(b).To(MatchRegexp(`^[0-9a-f]{32}$`))
		Expect(b).NotTo(Equal(a))
	})
})

var _ = Describe("RolePassword", func() {
	It("returns the hex value carried by the environment variable", func() {
		GinkgoT().Setenv(dbtest.AppUserPasswordEnv, "0123abcd")
		Expect(dbtest.RolePassword(dbtest.AppUserPasswordEnv)).To(Equal("0123abcd"))
	})

	DescribeTable("rejects a missing or unsafe value, naming the variable and the fix",
		func(value, want string) {
			GinkgoT().Setenv(dbtest.PurgeUserPasswordEnv, value)
			pw, err := dbtest.RolePassword(dbtest.PurgeUserPasswordEnv)
			Expect(pw).To(BeEmpty())
			Expect(err).To(MatchError(ContainSubstring(dbtest.PurgeUserPasswordEnv)))
			Expect(err).To(MatchError(ContainSubstring(want)))
		},
		Entry("unset", "", "task test:integration"),
		Entry("a single quote", "ab'cd", "non-hex"),
		Entry("odd-length hex", "abc", "non-hex"),
		Entry("a SQL breakout", "x'; DROP ROLE app_user; --", "non-hex"),
	)
})

var _ = Describe("AlterRolePasswordSQL", func() {
	It("quotes the role and interpolates a hex password", func() {
		Expect(dbtest.AlterRolePasswordSQL("app_user", "0123abcd")).To(
			Equal(`ALTER ROLE "app_user" WITH PASSWORD '0123abcd'`))
	})

	It("quotes a role name that needs it", func() {
		Expect(dbtest.AlterRolePasswordSQL(`we"ird`, "ab")).To(
			Equal(`ALTER ROLE "we""ird" WITH PASSWORD 'ab'`))
	})

	DescribeTable("refuses a password that is not hex, naming the role and the fix",
		func(password string) {
			stmt, err := dbtest.AlterRolePasswordSQL("app_user", password)
			Expect(stmt).To(BeEmpty())
			Expect(err).To(MatchError(ContainSubstring("set app_user password")))
			Expect(err).To(MatchError(ContainSubstring("dbtest.NewRolePassword")))
		},
		Entry("empty", ""),
		Entry("a single quote", "ab'cd"),
		Entry("a semicolon", "ab;cd"),
		Entry("odd-length hex", "abc"),
	)
})

var _ = Describe("HardDeleteTenantSQL", func() {
	It("quotes a valid tenant ID into every statement", func() {
		stmt, err := dbtest.HardDeleteTenantSQL("acme-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(stmt).To(ContainSubstring("session_replication_role = replica"))
		Expect(stmt).To(ContainSubstring("'acme-1'"))
		Expect(stmt).To(ContainSubstring("'crosscodex_acme-1'"))
	})

	DescribeTable("refuses an invalid tenant ID with an actionable message",
		func(id string) {
			stmt, err := dbtest.HardDeleteTenantSQL(id)
			Expect(err).To(MatchError(ContainSubstring("tenant ID")))
			Expect(stmt).To(BeEmpty())
		},
		Entry("empty", ""),
		Entry("quote injection", "a'; DROP TABLE tenants; --"),
		Entry("uppercase", "Acme"),
	)
})

var _ = Describe("RoleDSN", func() {
	const rolePassword = "0123456789abcdef"

	It("replaces the userinfo and keeps the host, port, database and query", func() {
		dsn, err := dbtest.RoleDSN(
			"postgres://postgres:su-secret@db.example:5433/crosscodex_test?sslmode=verify-full&sslrootcert=%2Fcerts%2Fca.pem",
			"app_user", rolePassword)
		Expect(err).NotTo(HaveOccurred())
		u, err := url.Parse(dsn)
		Expect(err).NotTo(HaveOccurred())
		Expect(u.User.Username()).To(Equal("app_user"))
		password, set := u.User.Password()
		Expect(set).To(BeTrue())
		Expect(password).To(Equal(rolePassword))
		Expect(u.Scheme).To(Equal("postgres"))
		Expect(u.Host).To(Equal("db.example:5433"))
		Expect(u.Path).To(Equal("/crosscodex_test"))
		Expect(u.Query()).To(Equal(url.Values{"sslmode": {"verify-full"}, "sslrootcert": {"/certs/ca.pem"}}))
		Expect(dsn).NotTo(ContainSubstring("su-secret"))
	})

	It("accepts a postgresql:// URL with no host, as a Unix-socket DSN has", func() {
		dsn, err := dbtest.RoleDSN("postgresql:///crosscodex?host=/run/postgresql", "app_user", rolePassword)
		Expect(err).NotTo(HaveOccurred())
		Expect(dsn).To(Equal("postgresql://app_user:" + rolePassword + "@/crosscodex?host=/run/postgresql"))
	})

	DescribeTable("refuses a password that is not hex without echoing it or the DSN, naming the role and the fix",
		func(password string) {
			dsn, err := dbtest.RoleDSN("postgres://postgres:su-secret@db:5432/x", "purge_user", password)
			Expect(dsn).To(BeEmpty())
			Expect(err).To(MatchError(ContainSubstring("build purge_user DSN")))
			Expect(err).To(MatchError(ContainSubstring("dbtest.NewRolePassword")))
			Expect(err.Error()).NotTo(ContainSubstring("su-secret"))
			if password != "" {
				Expect(err.Error()).NotTo(ContainSubstring(password))
			}
		},
		Entry("empty", ""),
		Entry("a single quote", "ab'cd"),
		Entry("odd-length hex", "abc"),
		Entry("userinfo smuggling", "x@evil:5432/"),
	)

	DescribeTable("refuses a DSN that is not a postgres URL without echoing it or the password",
		func(input string) {
			dsn, err := dbtest.RoleDSN(input, "app_user", rolePassword)
			Expect(dsn).To(BeEmpty())
			Expect(err).To(MatchError(ContainSubstring("build app_user DSN: the DSN passed to RoleDSN")))
			Expect(err).To(MatchError(ContainSubstring("postgres://user:password@host:port/dbname")))
			Expect(err.Error()).NotTo(ContainSubstring("s3cret"))
			Expect(err.Error()).NotTo(ContainSubstring(rolePassword))
		},
		Entry("an invalid port", "postgres://su:s3cret@db:port/x"),
		Entry("an invalid escape in the password", "postgres://su:s3cret%zz@db:5432/x"),
		Entry("a control character", "postgres://su:s3cret@db:5432/x\x7f"),
		Entry("a keyword/value DSN", "host=db user=su password=s3cret dbname=x"),
		Entry("an opaque URL, whose userinfo String drops", "postgres:s3cret@db/x"),
		Entry("another scheme", "s3cret://su:s3cret@db:5432/x"),
		Entry("empty", ""),
	)
})

var _ = Describe("SetRolePassword", func() {
	It("refuses a non-hex password before connecting, naming the role and the fix", func() {
		err := dbtest.SetRolePassword(context.Background(), "postgres://su:s3cret@127.0.0.1:1/x", "purge_user", "ab'cd")
		Expect(err).To(MatchError(ContainSubstring("set purge_user password")))
		Expect(err).To(MatchError(ContainSubstring("dbtest.NewRolePassword")))
		Expect(err.Error()).NotTo(ContainSubstring("connect"))
	})

	It("reports a failed admin connection without the admin password", func() {
		err := dbtest.SetRolePassword(context.Background(), "postgres://su:s3cret@127.0.0.1:1/x?connect_timeout=2", "app_user", "00ff")
		Expect(err).To(MatchError(ContainSubstring("set app_user password: connect as the admin role")))
		Expect(err.Error()).NotTo(ContainSubstring("s3cret"))
	})
})

var _ = Describe("TestID", func() {
	It("returns the prefix, a dash and 8 hex characters, fresh on every call", func() {
		a, b := dbtest.TestID("commit-fail"), dbtest.TestID("commit-fail")
		Expect(a).To(MatchRegexp(`^commit-fail-[0-9a-f]{8}$`))
		Expect(b).To(MatchRegexp(`^commit-fail-[0-9a-f]{8}$`))
		Expect(b).NotTo(Equal(a))
	})
})

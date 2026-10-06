package dbtest_test

import (
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

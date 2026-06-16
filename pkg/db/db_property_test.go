package db_test

import (
	"net/url"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/db"
)

var _ = Describe("Property Specifications", Ordered, func() {
	Context("redactDSN — credential leakage prevention", func() {
		It("never leaks passwords in URI-style DSNs", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				user := rapid.StringMatching(`[a-z]{3,10}`).Draw(t, "user")
				password := rapid.StringMatching(`[a-zA-Z0-9!@#$%^&*]{5,20}`).Draw(t, "password")
				host := rapid.StringMatching(`[a-z]{3,10}`).Draw(t, "host")
				dbname := rapid.StringMatching(`[a-z]{3,10}`).Draw(t, "dbname")
				dsn := "postgresql://" + url.UserPassword(user, password).String() + "@" + host + ":5432/" + dbname
				redacted := db.ExportRedactDSN(dsn)
				if strings.Contains(redacted, password) {
					t.Fatalf("redactDSN leaked password in URI DSN: input=%q output=%q", dsn, redacted)
				}
				if !strings.Contains(redacted, "REDACTED") {
					t.Fatalf("redactDSN did not insert REDACTED marker: %q", redacted)
				}
			})
		})

		It("is idempotent", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				dsn := rapid.StringMatching(`[a-zA-Z0-9:/@._=-]{0,100}`).Draw(t, "dsn")
				once := db.ExportRedactDSN(dsn)
				twice := db.ExportRedactDSN(once)
				if once != twice {
					t.Fatalf("redactDSN not idempotent: once=%q twice=%q", once, twice)
				}
			})
		})
	})
})

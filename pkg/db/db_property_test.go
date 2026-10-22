package db_test

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

var _ = Describe("Property Specifications", Ordered, func() {
	It("SCRAMVerifier output is well-formed, quote-free, deterministic and salt-dependent", func() {
		shape := regexp.MustCompile(`^SCRAM-SHA-256\$4096:[A-Za-z0-9+/=]+\$[A-Za-z0-9+/=]+:[A-Za-z0-9+/=]+$`)
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			password := rapid.StringMatching(`[!-~]{1,64}`).Draw(t, "password")
			salt := rapid.SliceOfN(rapid.Byte(), 16, 16).Draw(t, "salt")
			other := append([]byte(nil), salt...)
			other[0]++
			v, err := db.SCRAMVerifier(password, salt)
			if err != nil {
				t.Fatalf("printable ASCII password %q refused: %v", password, err)
			}
			if !shape.MatchString(v) || strings.Contains(v, "'") {
				t.Fatalf("malformed verifier %q", v)
			}
			again, err := db.SCRAMVerifier(password, salt)
			if err != nil {
				t.Fatalf("second call refused %q: %v", password, err)
			}
			changed, err := db.SCRAMVerifier(password, other)
			if err != nil {
				t.Fatalf("call with changed salt refused %q: %v", password, err)
			}
			if again != v || changed == v {
				t.Fatalf("verifier not deterministic or not salt-dependent for %q", password)
			}
		})
	})

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

	It("AdvisoryLockKey is deterministic for any name", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			name := rapid.String().Draw(t, "name")
			if db.AdvisoryLockKey(name) != db.AdvisoryLockKey(name) { //nolint:staticcheck // SA4000: two independent calls, verifying they agree is the point
				t.Fatalf("AdvisoryLockKey(%q) is not deterministic", name)
			}
		})
	})
	Context("ValidatePseudonym", func() {
		It("accepts every well-formed pseudonym and refuses it with any uppercase letter", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				kind := rapid.SampledFrom([]string{db.IdentityKindTenant, db.IdentityKindReviewer}).Draw(t, "kind")
				p := "hidden-" + kind + "-" + rapid.StringMatching(`[0-9a-f]{16}`).Draw(t, "hex")
				if err := db.ValidatePseudonym(p); err != nil {
					t.Fatalf("well-formed pseudonym %q refused: %v", p, err)
				}
				i := rapid.IntRange(0, len(p)-1).Draw(t, "index")
				upper := rapid.ByteRange('A', 'Z').Draw(t, "upper")
				mutated := p[:i] + string(upper) + p[i+1:]
				if db.ValidatePseudonym(mutated) == nil {
					t.Fatalf("pseudonym with uppercase %q at %d accepted: %q", upper, i, mutated)
				}
			})
		})
	})

	Context("TenantAdmin ID pre-validation", func() {
		It("sends an ID to the database exactly when pkg/tenant accepts it", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				id := rapid.OneOf(
					rapid.StringMatching(`[a-z][a-z0-9-]{1,50}[a-z0-9]`),
					rapid.StringMatching(`hidden-[a-z0-9-]{0,40}[a-z0-9]`),
					rapid.String(),
				).Draw(t, "id")
				pool, calls := db.ExportNewFailingPool()
				admin, err := db.ExportNewTenantAdmin(pool, nil, nil)
				if err != nil {
					t.Fatalf("new TenantAdmin: %v", err)
				}
				_, err = admin.Provision(context.Background(), id, "Name")
				// IDs starting with "hidden-" are reserved for pseudonyms (#195).
				if tenant.ValidateTenantID(id) == nil && !strings.HasPrefix(id, "hidden-") {
					if !errors.Is(err, db.ErrExportPoolUsed) || calls() != 1 {
						t.Fatalf("valid ID %q: want one query, got %d calls, err %v", id, calls(), err)
					}
					return
				}
				if !errors.Is(err, tenant.ErrInvalidTenant) || calls() != 0 {
					t.Fatalf("invalid ID %q reached the database (%d calls) or was not rejected as invalid: %v", id, calls(), err)
				}
			})
		})
	})
})

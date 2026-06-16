package authn_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"time"

	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/authn"
)

func generateTestCertForProperty(cn, org, ou string) *x509.Certificate {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName:         cn,
			Organization:       []string{org},
			OrganizationalUnit: []string{ou},
		},
		NotBefore: time.Now().Add(-time.Hour),
		NotAfter:  time.Now().Add(time.Hour),
	}
	certDER, _ := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	cert, _ := x509.ParseCertificate(certDER)
	return cert
}

var _ = Describe("Property Specifications", Ordered, func() {
	Context("matchCert — AND semantics with isEmpty", func() {
		It("matches every certificate when all match fields are empty (isEmpty behavior)", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				cn := rapid.StringMatching(`[a-z]{3,10}`).Draw(t, "cn")
				org := rapid.StringMatching(`[a-z]{3,10}`).Draw(t, "org")
				cert := generateTestCertForProperty(cn, org, "unit")
				emptyMatch := authn.X509Match{}
				result := authn.MatchCert(cert, emptyMatch)
				if !result {
					t.Fatalf("empty match should match any cert, got false for CN=%q", cn)
				}
			})
		})

		It("rejects when any non-empty field does not match", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				cn := rapid.StringMatching(`[a-z]{3,10}`).Draw(t, "cn")
				cert := generateTestCertForProperty(cn, "myorg", "unit")
				// Set CN to something that won't match
				wrongCN := rapid.StringMatching(`[A-Z]{3,10}`).Draw(t, "wrong-cn")
				match := authn.X509Match{CN: wrongCN}
				result := authn.MatchCert(cert, match)
				if result {
					t.Fatalf("matchCert should reject when CN %q does not match cert CN %q", wrongCN, cn)
				}
			})
		})
	})

	Context("RequireRole — fail-closed", func() {
		It("always returns error when zero roles are required", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				nRoles := rapid.IntRange(0, 5).Draw(t, "nRoles")
				roles := make([]string, nRoles)
				for i := range roles {
					roles[i] = rapid.StringMatching(`[a-z]{3,10}`).Draw(t, "role")
				}
				identity := authn.Identity{
					TenantID: "test-tenant",
					Subject:  "test-subject",
					Method:   "x509",
					Roles:    roles,
				}
				// Zero required roles — must always fail
				err := authn.RequireRole(identity)
				if err == nil {
					t.Fatalf("RequireRole with zero required roles returned nil for identity with roles %v", roles)
				}
			})
		})

		It("succeeds when identity has a matching role", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				role := rapid.StringMatching(`[a-z]{3,10}`).Draw(t, "role")
				extra := rapid.SliceOfN(rapid.StringMatching(`[a-z]{3,10}`), 0, 5).Draw(t, "extras")
				identity := authn.Identity{
					TenantID: "test-tenant",
					Subject:  "test-subject",
					Method:   "x509",
					Roles:    append([]string{role}, extra...),
				}
				err := authn.RequireRole(identity, role)
				if err != nil {
					t.Fatalf("RequireRole should succeed when identity has role %q: %v", role, err)
				}
			})
		})
	})
})

package authn

import (
	"context"
	"crypto/x509"
	"fmt"
	"path/filepath"

	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// X509Config configures the X.509 mTLS authenticator.
type X509Config struct {
	SingleTenant  bool          // true when tenants.enabled == false
	DefaultTenant string        // tenant ID for single-tenant mode
	Mappings      []X509Mapping // ordered mapping rules (multi-tenant mode)
}

// X509Mapping maps a certificate field pattern to a tenant and roles.
type X509Mapping struct {
	Match  X509Match // Certificate field matchers (AND semantics)
	Tenant string    // Target tenant ID
	Roles  []string  // Assigned roles
}

// X509Match holds glob patterns for X.509 certificate fields.
// All non-empty fields must match (AND semantics).
// Uses filepath.Match for glob evaluation; note that '*' does not
// match '/' in filepath.Match, so SPIFFE URIs require per-segment
// wildcards or exact matches rather than a single trailing '*'.
type X509Match struct {
	CN           string // Glob pattern for Subject.CommonName
	Organization string // Glob pattern for Subject.Organization[0]
	OrgUnit      string // Glob pattern for Subject.OrganizationalUnit[0]
	SANEmail     string // Glob pattern for EmailAddresses (any match)
	SANDNS       string // Glob pattern for DNSNames (any match)
	SANURI       string // Glob pattern for URIs (any match, string form)
}

// isEmpty returns true when all match fields are empty, meaning
// the match would accept any certificate.
func (m X509Match) isEmpty() bool {
	return m.CN == "" && m.Organization == "" && m.OrgUnit == "" &&
		m.SANEmail == "" && m.SANDNS == "" && m.SANURI == ""
}

// X509Authenticator authenticates requests using X.509 client certificates.
type X509Authenticator struct {
	config X509Config
}

// NewX509Authenticator creates an X509Authenticator with the given config.
// Returns an error if tenant IDs in the config are invalid.
func NewX509Authenticator(cfg X509Config) (*X509Authenticator, error) {
	// Validate tenant IDs at construction time (fail fast)
	if cfg.SingleTenant {
		if err := tenant.ValidateTenantID(cfg.DefaultTenant); err != nil {
			return nil, fmt.Errorf("invalid default tenant in X509 config: %w", err)
		}
	} else {
		for i, m := range cfg.Mappings {
			if err := tenant.ValidateTenantID(m.Tenant); err != nil {
				return nil, fmt.Errorf("invalid tenant in X509 mapping %d: %w", i, err)
			}
			if m.Match.isEmpty() {
				return nil, fmt.Errorf("X509 mapping %d has empty match (would match all certificates): configure at least one match field", i)
			}
		}
	}
	return &X509Authenticator{config: cfg}, nil
}

// Authenticate extracts identity from a client certificate.
//
// SECURITY NOTE: This authenticator does not perform CRL or OCSP revocation
// checking. A compromised or revoked certificate remains valid until it
// expires. Go's crypto/tls does not check revocation by default. When
// revocation checking is required, add an optional RevocationChecker to
// X509Config or perform revocation checks at the TLS termination layer
// (e.g., nginx, Envoy).
func (a *X509Authenticator) Authenticate(_ context.Context, req *Request) (*Identity, error) {
	if req.TLSState == nil {
		return nil, ErrUnsupportedMethod
	}
	if len(req.TLSState.PeerCertificates) == 0 {
		return nil, fmt.Errorf("client certificate required but not provided: %w", ErrAuthenticationFailed)
	}

	leaf := req.TLSState.PeerCertificates[0]

	// Single-tenant mode: any CA-signed cert gets default tenant + admin
	if a.config.SingleTenant {
		return &Identity{
			Subject:  leaf.Subject.CommonName,
			TenantID: a.config.DefaultTenant,
			Roles:    []string{"admin"},
			Method:   AuthMethodMTLS,
			Claims:   certClaims(leaf),
		}, nil
	}

	// Multi-tenant mode: iterate mappings, first match wins
	for _, m := range a.config.Mappings {
		if matchCert(leaf, m.Match) {
			// Defense in depth: validate tenant ID at auth time too
			if err := tenant.ValidateTenantID(m.Tenant); err != nil {
				return nil, fmt.Errorf("mapping resolved to invalid tenant %q: %w", m.Tenant, err)
			}
			return &Identity{
				Subject:  leaf.Subject.CommonName,
				TenantID: m.Tenant,
				Roles:    m.Roles,
				Method:   AuthMethodMTLS,
				Claims:   certClaims(leaf),
			}, nil
		}
	}

	return nil, fmt.Errorf("no mapping matched certificate CN=%q, O=%v: %w",
		leaf.Subject.CommonName, leaf.Subject.Organization, ErrAuthenticationFailed)
}

// SupportedMethods returns the authentication methods handled by this authenticator.
func (a *X509Authenticator) SupportedMethods() []AuthMethod {
	return []AuthMethod{AuthMethodMTLS}
}

// matchCert evaluates an X509Match against a certificate.
// All non-empty fields must match (AND semantics).
func matchCert(cert *x509.Certificate, m X509Match) bool {
	if m.CN != "" && !globMatch(m.CN, cert.Subject.CommonName) {
		return false
	}
	if m.Organization != "" && !matchFirst(m.Organization, cert.Subject.Organization) {
		return false
	}
	if m.OrgUnit != "" && !matchFirst(m.OrgUnit, cert.Subject.OrganizationalUnit) {
		return false
	}
	if m.SANEmail != "" && !matchAny(m.SANEmail, cert.EmailAddresses) {
		return false
	}
	if m.SANDNS != "" && !matchAny(m.SANDNS, cert.DNSNames) {
		return false
	}
	if m.SANURI != "" && !matchAnyURI(m.SANURI, cert) {
		return false
	}
	return true
}

// globMatch wraps filepath.Match, treating errors as non-match.
func globMatch(pattern, value string) bool {
	matched, err := filepath.Match(pattern, value)
	return err == nil && matched
}

// matchFirst matches a glob pattern against the first element of a slice.
// Returns false if the slice is empty.
func matchFirst(pattern string, values []string) bool {
	if len(values) == 0 {
		return false
	}
	return globMatch(pattern, values[0])
}

// matchAny returns true if the pattern matches any element in the slice.
func matchAny(pattern string, values []string) bool {
	for _, v := range values {
		if globMatch(pattern, v) {
			return true
		}
	}
	return false
}

// matchAnyURI returns true if the pattern matches any URI SAN (as string).
func matchAnyURI(pattern string, cert *x509.Certificate) bool {
	for _, u := range cert.URIs {
		if globMatch(pattern, u.String()) {
			return true
		}
	}
	return false
}

// certClaims extracts method-specific claims from a certificate for the Identity.
func certClaims(cert *x509.Certificate) map[string]any {
	claims := map[string]any{
		"serial": cert.SerialNumber.String(),
		"issuer": cert.Issuer.CommonName,
	}
	if len(cert.DNSNames) > 0 {
		claims["dns_names"] = cert.DNSNames
	}
	return claims
}

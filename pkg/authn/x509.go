package authn

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// X509Config configures the X.509 mTLS authenticator.
type X509Config struct {
	SingleTenant  bool          // true when tenants.enabled == false: every identity gets DefaultTenant
	DefaultTenant string        // tenant ID for single-tenant mode
	Mappings      []X509Mapping // ordered mapping rules, first match wins (required, both modes)
}

// X509Mapping maps a certificate field pattern to a tenant and roles.
type X509Mapping struct {
	Match  X509Match // Certificate field matchers (AND semantics)
	Tenant string    // Target tenant ID (single-tenant: empty or DefaultTenant)
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
//
// Roles come only from Mappings, in both modes, so construction fails closed:
// it errors on zero mappings, an empty match, a role outside KnownRoles and an
// invalid tenant. In single-tenant mode a mapping's Tenant must be empty or
// equal DefaultTenant; the identity's tenant is always DefaultTenant.
func NewX509Authenticator(cfg X509Config) (*X509Authenticator, error) {
	if cfg.SingleTenant {
		if err := tenant.ValidateTenantID(cfg.DefaultTenant); err != nil {
			return nil, fmt.Errorf("invalid default tenant in X509 config: %w", err)
		}
	}
	if len(cfg.Mappings) == 0 {
		return nil, errors.New("no X509 mappings configured: add at least one entry to auth.x509_mappings; without one no certificate is granted any role")
	}
	for i, m := range cfg.Mappings {
		if cfg.SingleTenant {
			// Name no tenant in the message: a mapping for another tenant is a
			// misconfiguration, and the value is not ours to echo.
			if m.Tenant != "" && m.Tenant != cfg.DefaultTenant {
				return nil, fmt.Errorf("X509 mapping %d names a tenant other than the default: in single-tenant mode leave tenant empty or set it to the default tenant", i)
			}
		} else if err := tenant.ValidateTenantID(m.Tenant); err != nil {
			return nil, fmt.Errorf("invalid tenant in X509 mapping %d: %w", i, err)
		}
		if m.Match.isEmpty() {
			return nil, fmt.Errorf("X509 mapping %d has empty match (would match all certificates): configure at least one match field", i)
		}
		for _, role := range m.Roles {
			if !slices.Contains(KnownRoles, role) {
				return nil, fmt.Errorf("X509 mapping %d grants unknown role %q: use one of %s", i, role, strings.Join(KnownRoles, ", "))
			}
		}
	}
	return &X509Authenticator{config: cfg}, nil
}

// Authenticate extracts identity from a client certificate. The first mapping
// whose Match accepts the leaf certificate decides roles (and, in multi-tenant
// mode, the tenant); no match is ErrAuthenticationFailed.
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

	for _, m := range a.config.Mappings {
		if !m.Match.Matches(leaf) {
			continue
		}
		tenantID := a.config.DefaultTenant
		if !a.config.SingleTenant {
			tenantID = m.Tenant
			// Defense in depth: validate tenant ID at auth time too
			if err := tenant.ValidateTenantID(tenantID); err != nil {
				return nil, fmt.Errorf("mapping resolved to invalid tenant %q: %w", tenantID, err)
			}
		}
		return &Identity{
			Subject:  leaf.Subject.CommonName,
			TenantID: tenantID,
			Roles:    m.Roles,
			Method:   AuthMethodMTLS,
			Claims:   certClaims(leaf),
		}, nil
	}

	return nil, fmt.Errorf("no mapping matched certificate CN=%q, O=%v: %w",
		leaf.Subject.CommonName, leaf.Subject.Organization, ErrAuthenticationFailed)
}

// SupportedMethods returns the authentication methods handled by this authenticator.
func (a *X509Authenticator) SupportedMethods() []AuthMethod {
	return []AuthMethod{AuthMethodMTLS}
}

// Matches reports whether cert satisfies every non-empty field (AND
// semantics). An empty match accepts any certificate; constructors that take
// mappings reject empty matches rather than rely on this.
func (m X509Match) Matches(cert *x509.Certificate) bool {
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

package authn

// AuthMethod identifies the authentication method used.
type AuthMethod string

const (
	// AuthMethodMTLS indicates mutual TLS authentication.
	AuthMethodMTLS AuthMethod = "mtls"

	// AuthMethodKerberos indicates Kerberos authentication.
	AuthMethodKerberos AuthMethod = "kerberos"

	// AuthMethodSAML indicates SAML assertion authentication.
	AuthMethodSAML AuthMethod = "saml"
)

// Identity represents an authenticated user or service.
type Identity struct {
	Subject  string         // Unique identifier (DN, UPN, or SAML NameID)
	TenantID string         // Tenant identifier
	Method   AuthMethod     // Authentication method used
	Claims   map[string]any // Additional claims (roles, attributes, etc.)
}

// Request represents an authentication request.
type Request struct {
	Method      AuthMethod        // Requested authentication method
	Credentials map[string]any    // Method-specific credentials
	Metadata    map[string]string // Request metadata (headers, etc.)
}

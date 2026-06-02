package authn

import (
	"crypto/tls"
	"time"
)

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

const (
	// RoleAdmin is the well-known admin role identifier.
	RoleAdmin = "admin"
)

// Identity represents an authenticated user or service.
type Identity struct {
	Subject  string         // Who: CN, email, or principal name
	TenantID string         // Resolved tenant for this identity
	Roles    []string       // Assigned roles
	Method   AuthMethod     // How they authenticated
	Claims   map[string]any // Method-specific claims
}

// Request represents an authentication request.
type Request struct {
	Method    AuthMethod           // Which method to attempt (or empty for auto)
	TLSState  *tls.ConnectionState // For X.509: peer certificates
	Headers   map[string]string    // For SAML/OIDC: bearer tokens, assertions
	Body      []byte               // For SAML: POST body
	ClientIP  string               // Remote address for audit
	SessionID string               // Correlation ID for audit trail
}

// AuthEvent records an authentication attempt for audit purposes.
type AuthEvent struct {
	Timestamp     time.Time         // When the attempt occurred
	Principal     string            // Attempted identity (cert CN, etc.)
	TenantID      string            // Resolved tenant (or "unknown")
	Roles         []string          // Assigned roles (empty on failure)
	Method        AuthMethod        // Authentication method used
	ClientIP      string            // Remote address
	Success       bool              // Whether authentication succeeded
	FailureReason string            // Human-readable reason (empty on success)
	SessionID     string            // Correlation ID
	Details       map[string]string // Method-specific details (cert serial, etc.)
}

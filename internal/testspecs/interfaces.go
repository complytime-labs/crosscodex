package testspecs

import "context"

// TenantIsolatedComponent defines contract for components that enforce tenant isolation.
//
// Components implementing this interface must ensure that:
// - All operations are scoped to a specific tenant
// - Tenant context is properly propagated through the component
// - Cross-tenant data access is prevented
// - Tenant ID validation follows CrossCodex standards
//
// This interface enables shared behavior specifications for tenant isolation
// testing, ensuring consistent multi-tenant boundaries across all components.
type TenantIsolatedComponent interface {
	// WithTenantContext returns a new instance of the component configured
	// for the tenant specified in the context. The returned component should
	// only operate on data belonging to that tenant.
	WithTenantContext(ctx context.Context) TenantIsolatedComponent

	// ValidateTenantAccess verifies that the given tenant ID is valid and
	// accessible by the current component. Returns an error if the tenant
	// ID is invalid, inaccessible, or violates isolation policies.
	ValidateTenantAccess(tenantID string) error

	// GetTenantID extracts and returns the tenant ID from the given context.
	// Returns an error if no tenant context is available or if the tenant
	// ID is invalid.
	GetTenantID(ctx context.Context) (string, error)
}

// ConfigurableComponent defines contract for components that load and validate configuration.
//
// Components implementing this interface must ensure that:
// - Configuration loading follows XDG Base Directory specification
// - Configuration validation is comprehensive and actionable
// - Configuration precedence is handled correctly (env vars > config file > defaults)
// - Invalid configuration produces clear error messages
//
// This interface enables shared behavior specifications for configuration
// testing, ensuring consistent configuration handling across all components.
type ConfigurableComponent interface {
	// LoadConfiguration loads configuration from the specified path.
	// The path should follow XDG conventions and support precedence resolution.
	// Returns an error if the configuration file is invalid or inaccessible.
	LoadConfiguration(path string) error

	// ValidateConfiguration validates the loaded configuration for completeness
	// and correctness. Returns an error with actionable feedback if validation fails.
	ValidateConfiguration() error

	// GetConfigValue retrieves a configuration value by key, applying precedence
	// rules (environment variables > config file > defaults).
	// Returns an error if the key is not found or the value is invalid.
	GetConfigValue(key string) (interface{}, error)
}

// SecureComponent defines contract for components that handle security boundaries.
//
// Components implementing this interface must ensure that:
// - All inputs are validated against injection attacks and malformed data
// - Outputs are properly sanitized to prevent information leakage
// - Security mode enforcement is consistent
// - FIPS compliance is maintained when enabled
//
// This interface enables shared behavior specifications for security testing,
// ensuring consistent security boundaries across all components.
type SecureComponent interface {
	// ValidateInput validates the provided input against security policies,
	// checking for injection attacks, malformed data, and size limits.
	// Returns an error if the input is invalid or potentially malicious.
	ValidateInput(input interface{}) error

	// SanitizeOutput sanitizes the provided output to prevent information
	// leakage, removing sensitive data and applying output encoding.
	// Returns the sanitized version of the output.
	SanitizeOutput(output interface{}) interface{}

	// IsSecureModeEnabled returns true if the component is operating in
	// secure mode (e.g., FIPS mode, enhanced validation, stricter policies).
	IsSecureModeEnabled() bool
}

// ErrorHandlingComponent defines contract for components that implement robust error handling.
//
// Components implementing this interface must ensure that:
// - Errors are handled defensively with proper context preservation
// - Retryable errors are identified correctly
// - Error state is tracked for debugging and audit purposes
// - Error recovery follows established patterns
//
// This interface enables shared behavior specifications for error handling
// testing, ensuring consistent error management across all components.
type ErrorHandlingComponent interface {
	// HandleError processes the provided error according to the component's
	// error handling policy. May perform logging, metric collection, or
	// error transformation. Returns the processed error.
	HandleError(err error) error

	// IsRetryableError determines if the provided error represents a condition
	// that can be retried. Returns true for transient errors (network timeouts,
	// temporary resource unavailability), false for permanent errors.
	IsRetryableError(err error) bool

	// GetLastError returns the most recent error encountered by the component.
	// Used for debugging and error state inspection. Returns nil if no error.
	GetLastError() error
}

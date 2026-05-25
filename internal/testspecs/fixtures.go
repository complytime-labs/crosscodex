package testspecs

import (
	"context"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// TenantFixture represents a test tenant with validation metadata
type TenantFixture struct {
	TenantID    string // The tenant identifier to test
	DisplayName string // Human-readable name for the test case
	Valid       bool   // Whether this tenant ID should pass validation
	ErrorType   string // Expected error type for invalid tenants
}

// StandardTenantContexts provides common tenant test cases for all packages
var StandardTenantContexts = map[string]TenantFixture{
	"valid-tenant": {
		TenantID:    "acme-corp",
		DisplayName: "Valid tenant with standard format",
		Valid:       true,
		ErrorType:   "",
	},
	"min-length": {
		TenantID:    "abc", // exactly 3 characters (minimum)
		DisplayName: "Minimum length valid tenant",
		Valid:       true,
		ErrorType:   "",
	},
	"max-length": {
		TenantID:    "a123456789b123456789c123456789d123456789e123456789f123456789g123", // exactly 64 characters (maximum)
		DisplayName: "Maximum length valid tenant",
		Valid:       true,
		ErrorType:   "",
	},
	"invalid-chars": {
		TenantID:    "bad-tenant!@#",
		DisplayName: "Invalid tenant with special characters",
		Valid:       false,
		ErrorType:   "invalid_format",
	},
	"too-short": {
		TenantID:    "ab", // only 2 characters
		DisplayName: "Tenant ID shorter than minimum length",
		Valid:       false,
		ErrorType:   "too_short",
	},
	"too-long": {
		TenantID:    "a123456789b123456789c123456789d123456789e123456789f123456789g1234", // 65 characters
		DisplayName: "Tenant ID longer than maximum length",
		Valid:       false,
		ErrorType:   "too_long",
	},
	"empty": {
		TenantID:    "",
		DisplayName: "Empty tenant ID",
		Valid:       false,
		ErrorType:   "empty",
	},
}

// ConfigPathFixture represents test configuration paths with precedence metadata
type ConfigPathFixture struct {
	Paths       []string // List of configuration file paths
	Priority    string   // Priority level: user-config, system-wide, local
	Description string   // Human-readable description
}

// StandardConfigPaths provides common configuration path scenarios
var StandardConfigPaths = map[string]ConfigPathFixture{
	"xdg-precedence": {
		Paths: []string{
			"$XDG_CONFIG_HOME/crosscodex/config.yml",
			"$HOME/.config/crosscodex/config.yml",
			"$HOME/.crosscodex.yml",
		},
		Priority:    "user-config",
		Description: "XDG Base Directory specification paths with user precedence",
	},
	"system-paths": {
		Paths: []string{
			"/etc/crosscodex/config.yml",
			"/usr/local/etc/crosscodex/config.yml",
		},
		Priority:    "system-wide",
		Description: "System-wide configuration paths",
	},
	"local-files": {
		Paths: []string{
			"./crosscodex.yml",
			"./config/crosscodex.yml",
			"./.crosscodex.yml",
		},
		Priority:    "local",
		Description: "Local project configuration files",
	},
}

// ErrorCondition represents a test error scenario with categorization
type ErrorCondition struct {
	Message    string // Error message text
	Category   string // Error category: network, authorization, validation, client, server
	Retryable  bool   // Whether this error should be retried
	Code       int    // Optional error code
	Underlying error  // Optional underlying error for wrapping
}

// StandardErrors provides common error scenarios for testing
var StandardErrors = map[string]ErrorCondition{
	"network-timeout": {
		Message:   "connection timeout after 30s",
		Category:  "network",
		Retryable: true,
		Code:      0,
	},
	"permission-denied": {
		Message:   "access denied: insufficient permissions",
		Category:  "authorization",
		Retryable: false,
		Code:      403,
	},
	"invalid-input": {
		Message:   "invalid input: field 'email' is required",
		Category:  "validation",
		Retryable: false,
		Code:      400,
	},
	"not-found": {
		Message:   "resource not found",
		Category:  "client",
		Retryable: false,
		Code:      404,
	},
}

// Context creation helpers for test setup

// CreateTenantContext creates a context with the specified tenant ID
func CreateTenantContext(tenantID string) context.Context {
	return tenant.WithTenant(context.Background(), tenantID)
}

// CreateTimeoutContext creates a context with the specified timeout
func CreateTimeoutContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), timeout)
}

// CreateCancelableContext creates a cancelable context
func CreateCancelableContext() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}

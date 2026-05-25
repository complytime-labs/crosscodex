package testspecs_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	. "github.com/onsi/ginkgo/v2"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// Note: Ginkgo specs are automatically included in the main test suite
// No separate TestSharedBehaviors function needed

var _ = Describe("SharedBehaviors", func() {
	Describe("TenantIsolationBehavior with mock component", testspecs.TenantIsolationBehavior(&behaviorTestTenantComponent{}))

	Describe("ConfigurationComplianceBehavior with mock component", testspecs.ConfigurationComplianceBehavior(&behaviorTestConfigComponent{}))

	Describe("SecurityBoundaryBehavior with mock component", testspecs.SecurityBoundaryBehavior(&behaviorTestSecureComponent{}))

	Describe("ErrorHandlingBehavior with mock component", testspecs.ErrorHandlingBehavior(&behaviorTestErrorComponent{}))
})

// Minimal test adapter implementations to test the behavior functions
type behaviorTestTenantComponent struct {
	tenantID      string
	accessErrors  map[string]error
	extractErrors map[context.Context]error
}

func (m *behaviorTestTenantComponent) WithTenantContext(ctx context.Context) testspecs.TenantIsolatedComponent {
	// Extract tenant ID from the context if possible
	if tenantID, err := tenant.FromContext(ctx); err == nil {
		return &behaviorTestTenantComponent{tenantID: tenantID}
	}
	// Return a component with a context-specific tenant ID
	return &behaviorTestTenantComponent{tenantID: "context-specific-tenant"}
}

func (m *behaviorTestTenantComponent) ValidateTenantAccess(tenantID string) error {
	if m.accessErrors != nil {
		if err, exists := m.accessErrors[tenantID]; exists {
			return err
		}
	}

	// Use the fixtures to determine validity
	for _, fixture := range testspecs.StandardTenantContexts {
		if fixture.TenantID == tenantID {
			if !fixture.Valid {
				return fmt.Errorf("invalid tenant format: %s", fixture.ErrorType)
			}
			return nil
		}
	}

	// Special case for forbidden tenant
	if tenantID == "forbidden-tenant" {
		return errors.New("access denied: tenant forbidden")
	}

	// Default to accepting unknown tenants (for basic testing)
	return nil
}

func (m *behaviorTestTenantComponent) GetTenantID(ctx context.Context) (string, error) {
	if m.extractErrors != nil {
		if err, exists := m.extractErrors[ctx]; exists {
			return "", err
		}
	}

	// Try to extract from context first
	if tenantID, err := tenant.FromContext(ctx); err == nil {
		return tenantID, nil
	}

	// Fall back to stored tenant ID
	if m.tenantID != "" {
		return m.tenantID, nil
	}
	return "default-tenant", nil
}

type behaviorTestConfigComponent struct {
	configData      map[string]interface{}
	configPath      string
	loadErrors      map[string]error
	validationError error
}

func (m *behaviorTestConfigComponent) LoadConfiguration(path string) error {
	m.configPath = path
	if m.loadErrors != nil {
		if err, exists := m.loadErrors[path]; exists {
			return err
		}
	}

	// Check for specific error cases
	if path == "/nonexistent/config.yml" {
		return fmt.Errorf("configuration file not found at %s: please check the path", path)
	}

	// Try to read the file content to detect malformed YAML
	if path != "" {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return fmt.Errorf("configuration file not found at %s: please check the path", path)
		}

		// Try to read file content
		content, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("failed to read configuration file: please check permissions")
		}

		// Basic YAML validation - look for malformed patterns
		contentStr := string(content)
		if strings.Contains(contentStr, "invalid yaml: [[[") ||
			strings.Contains(contentStr, "[[[") {
			return fmt.Errorf("invalid YAML format: please check syntax and structure")
		}
	}

	// Default successful load
	if m.configData == nil {
		m.configData = map[string]interface{}{
			"database": map[string]interface{}{"host": "localhost"},
			"nats":     map[string]interface{}{"url": "localhost:4222"},
			"storage":  map[string]interface{}{"type": "local"},
			"order":    "user-first",
		}
	}
	return nil
}

func (m *behaviorTestConfigComponent) ValidateConfiguration() error {
	return m.validationError
}

func (m *behaviorTestConfigComponent) GetConfigValue(key string) (interface{}, error) {
	if m.configData == nil {
		return nil, fmt.Errorf("configuration not loaded: please call LoadConfiguration first")
	}

	value, exists := m.configData[key]
	if !exists {
		return nil, fmt.Errorf("configuration key %q not found: please check available keys or verify the configuration file", key)
	}

	return value, nil
}

type behaviorTestSecureComponent struct {
	secureModeEnabled     bool
	inputValidationErrors map[interface{}]error
}

func (m *behaviorTestSecureComponent) ValidateInput(input interface{}) error {
	if m.inputValidationErrors != nil {
		if err, exists := m.inputValidationErrors[input]; exists {
			return err
		}
	}

	// Default validation logic
	if str, ok := input.(string); ok {
		if str == "malicious-input" {
			return fmt.Errorf("input contains malicious content: please sanitize input and try again")
		}
		if len(str) > 1000 {
			return fmt.Errorf("input exceeds maximum length of 1000 characters: please reduce input size")
		}
	}

	return nil
}

func (m *behaviorTestSecureComponent) SanitizeOutput(output interface{}) interface{} {
	if str, ok := output.(string); ok {
		// Simple sanitization - remove sensitive keywords completely
		if str == "password=secret123" {
			return "credentials=***" // Change key to avoid 'password' word
		}
		if str == "debug: sql query failed" {
			return "operation failed"
		}
	}

	if data, ok := output.(map[string]interface{}); ok {
		sanitized := make(map[string]interface{})
		for key, value := range data {
			// Sanitize sensitive keys by renaming them
			if key == "password" {
				sanitized["credentials"] = "***"
			} else if key == "secret" {
				sanitized["private"] = "***"
			} else {
				sanitized[key] = value
			}
		}
		return sanitized
	}

	return output
}

func (m *behaviorTestSecureComponent) IsSecureModeEnabled() bool {
	return m.secureModeEnabled
}

type behaviorTestErrorComponent struct {
	lastError       error
	retryableErrors map[error]bool
}

func (m *behaviorTestErrorComponent) HandleError(err error) error {
	m.lastError = err
	// Return the processed error
	return err
}

func (m *behaviorTestErrorComponent) IsRetryableError(err error) bool {
	if m.retryableErrors != nil {
		if retryable, exists := m.retryableErrors[err]; exists {
			return retryable
		}
	}

	// Default retry logic based on error message
	if err != nil {
		errMsg := err.Error()
		if errMsg == "connection timeout after 30s" {
			return true
		}
		if errMsg == "access denied: insufficient permissions" {
			return false
		}
	}

	return false
}

func (m *behaviorTestErrorComponent) GetLastError() error {
	return m.lastError
}

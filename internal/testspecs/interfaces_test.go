package testspecs_test

import (
	"context"
	"testing"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
)

// TestInterfaceCompilation verifies that all interface contracts compile
// and can be implemented by concrete types
func TestInterfaceCompilation(t *testing.T) {
	t.Run("TenantIsolatedComponent interface compiles", func(t *testing.T) {
		var _ testspecs.TenantIsolatedComponent = (*mockTenantIsolatedComponent)(nil)
	})

	t.Run("ConfigurableComponent interface compiles", func(t *testing.T) {
		var _ testspecs.ConfigurableComponent = (*mockConfigurableComponent)(nil)
	})

	t.Run("SecureComponent interface compiles", func(t *testing.T) {
		var _ testspecs.SecureComponent = (*mockSecureComponent)(nil)
	})

	t.Run("ErrorHandlingComponent interface compiles", func(t *testing.T) {
		var _ testspecs.ErrorHandlingComponent = (*mockErrorHandlingComponent)(nil)
	})
}

// Mock implementations to verify interface contracts are implementable

type mockTenantIsolatedComponent struct{}

func (m *mockTenantIsolatedComponent) WithTenantContext(ctx context.Context) testspecs.TenantIsolatedComponent {
	return m
}

func (m *mockTenantIsolatedComponent) ValidateTenantAccess(tenantID string) error {
	return nil
}

func (m *mockTenantIsolatedComponent) GetTenantID(ctx context.Context) (string, error) {
	return "test-tenant", nil
}

type mockConfigurableComponent struct{}

func (m *mockConfigurableComponent) LoadConfiguration(path string) error {
	return nil
}

func (m *mockConfigurableComponent) ValidateConfiguration() error {
	return nil
}

func (m *mockConfigurableComponent) GetConfigValue(key string) (interface{}, error) {
	return "test-value", nil
}

type mockSecureComponent struct{}

func (m *mockSecureComponent) ValidateInput(input interface{}) error {
	return nil
}

func (m *mockSecureComponent) SanitizeOutput(output interface{}) interface{} {
	return output
}

func (m *mockSecureComponent) IsSecureModeEnabled() bool {
	return true
}

type mockErrorHandlingComponent struct {
	lastError error
}

func (m *mockErrorHandlingComponent) HandleError(err error) error {
	m.lastError = err
	return err
}

func (m *mockErrorHandlingComponent) IsRetryableError(err error) bool {
	return false
}

func (m *mockErrorHandlingComponent) GetLastError() error {
	return m.lastError
}

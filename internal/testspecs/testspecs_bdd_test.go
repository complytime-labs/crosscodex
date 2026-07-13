package testspecs_test

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// ---------------------------------------------------------------------------
// Standard Fixtures
// ---------------------------------------------------------------------------

var _ = Describe("Standard Fixtures", func() {

	Context("Tenant Fixtures", func() {
		It("marks valid-tenant as Valid with a non-empty ID that passes pkg/tenant validation", func() {
			f := testspecs.StandardTenantContexts["valid-tenant"]
			Expect(f.Valid).To(BeTrue())
			Expect(f.TenantID).NotTo(BeEmpty())
			Expect(tenant.ValidateTenantID(f.TenantID)).To(Succeed())
		})

		It("marks invalid-chars as invalid with a non-empty ErrorType", func() {
			f := testspecs.StandardTenantContexts["invalid-chars"]
			Expect(f.Valid).To(BeFalse())
			Expect(f.ErrorType).NotTo(BeEmpty())
			Expect(tenant.ValidateTenantID(f.TenantID)).To(HaveOccurred())
		})

		It("has a min-length fixture with TenantID of exactly 3 characters", func() {
			f := testspecs.StandardTenantContexts["min-length"]
			Expect(f.Valid).To(BeTrue())
			Expect(f.TenantID).To(HaveLen(3))
		})

		It("has a max-length fixture with TenantID of exactly 64 characters", func() {
			f := testspecs.StandardTenantContexts["max-length"]
			Expect(f.Valid).To(BeTrue())
			Expect(f.TenantID).To(HaveLen(64))
		})

		It("marks too-short fixture as invalid with TenantID shorter than 3 chars", func() {
			f := testspecs.StandardTenantContexts["too-short"]
			Expect(f.Valid).To(BeFalse())
			Expect(len(f.TenantID)).To(BeNumerically("<", 3))
		})

		It("marks too-long fixture as invalid with TenantID longer than 64 chars", func() {
			f := testspecs.StandardTenantContexts["too-long"]
			Expect(f.Valid).To(BeFalse())
			Expect(len(f.TenantID)).To(BeNumerically(">", 64))
		})

		It("marks empty fixture as invalid with empty TenantID", func() {
			f := testspecs.StandardTenantContexts["empty"]
			Expect(f.Valid).To(BeFalse())
			Expect(f.TenantID).To(BeEmpty())
		})
	})

	Context("Context Creation", func() {
		It("SetupTenantContext produces a context with the correct tenant ID", func() {
			ctx := testspecs.SetupTenantContext("acme-corp")
			id, err := tenant.FromContext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(id).To(Equal("acme-corp"))
		})

		It("CreateTimeoutContext produces a context with a deadline", func() {
			timeout := 100 * time.Millisecond
			ctx, cancel := testspecs.CreateTimeoutContext(timeout)
			defer cancel()

			deadline, ok := ctx.Deadline()
			Expect(ok).To(BeTrue())
			Expect(time.Until(deadline)).To(BeNumerically("<=", timeout))
		})

		It("CreateCancelableContext is not cancelled initially and cancels on call", func() {
			ctx, cancel := testspecs.CreateCancelableContext()

			Consistently(ctx.Done()).ShouldNot(BeClosed())

			cancel()

			Eventually(ctx.Done()).Should(BeClosed())
		})
	})

	Context("Config Path Fixtures", func() {
		It("xdg-precedence has user-config priority and contains XDG config paths", func() {
			f := testspecs.StandardConfigPaths["xdg-precedence"]
			Expect(f.Paths).NotTo(BeEmpty())
			Expect(f.Priority).To(Equal("user-config"))

			hasXDGConfig := false
			for _, p := range f.Paths {
				if strings.Contains(p, "config") || strings.Contains(p, "XDG_CONFIG_HOME") {
					hasXDGConfig = true
					break
				}
			}
			Expect(hasXDGConfig).To(BeTrue(), "expected XDG config paths")
		})

		It("system-paths has system-wide priority and contains /etc or /usr", func() {
			f := testspecs.StandardConfigPaths["system-paths"]
			Expect(f.Priority).To(Equal("system-wide"))

			hasSystemPath := false
			for _, p := range f.Paths {
				if strings.HasPrefix(p, "/etc") || strings.HasPrefix(p, "/usr") {
					hasSystemPath = true
					break
				}
			}
			Expect(hasSystemPath).To(BeTrue(), "expected system paths like /etc or /usr")
		})

		It("local-files has local priority and contains relative paths", func() {
			f := testspecs.StandardConfigPaths["local-files"]
			Expect(f.Priority).To(Equal("local"))

			hasLocalPath := false
			for _, p := range f.Paths {
				if !filepath.IsAbs(p) {
					hasLocalPath = true
					break
				}
			}
			Expect(hasLocalPath).To(BeTrue(), "expected relative paths")
		})
	})

	Context("Error Conditions", func() {
		It("network-timeout is retryable with category 'network'", func() {
			e := testspecs.StandardErrors["network-timeout"]
			Expect(e.Retryable).To(BeTrue())
			Expect(e.Category).To(Equal("network"))
			Expect(e.Message).NotTo(BeEmpty())
		})

		It("permission-denied is not retryable with category 'authorization'", func() {
			e := testspecs.StandardErrors["permission-denied"]
			Expect(e.Retryable).To(BeFalse())
			Expect(e.Category).To(Equal("authorization"))
		})

		It("invalid-input is not retryable with category 'validation'", func() {
			e := testspecs.StandardErrors["invalid-input"]
			Expect(e.Retryable).To(BeFalse())
			Expect(e.Category).To(Equal("validation"))
		})

		It("not-found is not retryable with category 'client'", func() {
			e := testspecs.StandardErrors["not-found"]
			Expect(e.Retryable).To(BeFalse())
			Expect(e.Category).To(Equal("client"))
		})
	})
})

// ---------------------------------------------------------------------------
// Interface Contracts
// ---------------------------------------------------------------------------

// Compile-time interface satisfaction checks.
var (
	_ testspecs.TenantIsolatedComponent = (*mockTenantIsolatedComponent)(nil)
	_ testspecs.ConfigurableComponent   = (*mockConfigurableComponent)(nil)
	_ testspecs.SecureComponent         = (*mockSecureComponent)(nil)
	_ testspecs.ErrorHandlingComponent  = (*mockErrorHandlingComponent)(nil)
)

var _ = Describe("Interface Contracts", func() {
	It("TenantIsolatedComponent is implementable", func() {
		var c testspecs.TenantIsolatedComponent = &mockTenantIsolatedComponent{}
		Expect(c).NotTo(BeNil())
	})

	It("ConfigurableComponent is implementable", func() {
		var c testspecs.ConfigurableComponent = &mockConfigurableComponent{}
		Expect(c).NotTo(BeNil())
	})

	It("SecureComponent is implementable", func() {
		var c testspecs.SecureComponent = &mockSecureComponent{}
		Expect(c).NotTo(BeNil())
	})

	It("ErrorHandlingComponent is implementable", func() {
		var c testspecs.ErrorHandlingComponent = &mockErrorHandlingComponent{}
		Expect(c).NotTo(BeNil())
	})
})

// Mock implementations to verify interface contracts are implementable.

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

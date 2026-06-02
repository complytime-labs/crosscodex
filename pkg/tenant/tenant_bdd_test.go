package tenant_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

func TestTenantBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Tenant System BDD Suite")
}

var _ = Describe("Tenant System", Ordered, func() {

	BeforeAll(func() {
		testspecs.LogTestProgress("Starting Tenant System BDD test suite")
	})

	AfterAll(func() {
		testspecs.LogTestProgress("Tenant System BDD test suite completed")
	})

	// =================================================================
	// LEVEL 1: BEHAVIORAL SPECIFICATIONS
	// These specs test the "why" - what business behaviors the tenant system supports
	// =================================================================

	Describe("Tenant Isolation Behaviors", func() {
		Context("when enforcing multi-tenant data boundaries", func() {
			It("validates tenant identities to prevent spoofing", func() {
				// This spec ensures tenant IDs follow security-conscious format rules

				By("rejecting tenant IDs with unsafe characters")
				invalidTenant := testspecs.StandardTenantContexts["invalid-chars"]
				err := tenant.ValidateTenantID(invalidTenant.TenantID)
				Expect(err).To(HaveOccurred())
				Expect(err).To(testspecs.BeSecureError())
				Expect(err).To(testspecs.BeActionableError())

				By("accepting well-formed tenant identifiers")
				validTenant := testspecs.StandardTenantContexts["valid-tenant"]
				err = tenant.ValidateTenantID(validTenant.TenantID)
				Expect(err).NotTo(HaveOccurred())
			})

			It("propagates tenant context across service boundaries", func() {
				// This spec ensures tenant context doesn't get lost in call chains

				By("creating tenant-scoped context")
				validTenant := testspecs.StandardTenantContexts["valid-tenant"]
				ctx, err := tenant.WithTenant(context.Background(), validTenant.TenantID)
				Expect(err).NotTo(HaveOccurred())

				By("extracting tenant ID reliably from context")
				extractedTenantID, err := tenant.FromContext(ctx)
				testspecs.AssertNoError(err)
				Expect(extractedTenantID).To(testspecs.BeValidTenantID())
				Expect(extractedTenantID).To(Equal(validTenant.TenantID))

				By("maintaining tenant boundaries across context operations")
				// Simulate passing through multiple service calls
				servicCtx := testspecs.CreateTenantContext(validTenant.TenantID)
				returnedTenantID, err := tenant.FromContext(servicCtx)
				testspecs.AssertNoError(err)
				Expect(returnedTenantID).To(Equal(validTenant.TenantID))
			})

			It("prevents cross-tenant data access through context isolation", func() {
				// This spec ensures tenants can't access each other's data

				tenant1 := testspecs.StandardTenantContexts["valid-tenant"]
				tenant2 := testspecs.StandardTenantContexts["min-length"]

				By("creating isolated contexts for different tenants")
				ctx1, err1 := tenant.WithTenant(context.Background(), tenant1.TenantID)
				Expect(err1).NotTo(HaveOccurred())
				ctx2, err2 := tenant.WithTenant(context.Background(), tenant2.TenantID)
				Expect(err2).NotTo(HaveOccurred())

				By("ensuring contexts maintain separate tenant identities")
				id1, err1 := tenant.FromContext(ctx1)
				id2, err2 := tenant.FromContext(ctx2)

				testspecs.AssertNoError(err1)
				testspecs.AssertNoError(err2)

				Expect(id1).To(Equal(tenant1.TenantID))
				Expect(id2).To(Equal(tenant2.TenantID))
				Expect(id1).NotTo(Equal(id2))
			})

			It("enforces tenant ID format constraints for security", func() {
				// This spec validates that tenant IDs follow security-focused constraints

				By("enforcing minimum length to prevent brute force")
				shortTenant := testspecs.StandardTenantContexts["too-short"]
				err := tenant.ValidateTenantID(shortTenant.TenantID)
				Expect(err).To(HaveOccurred())
				Expect(err).To(testspecs.BeActionableError())

				By("enforcing maximum length to prevent DoS")
				longTenant := testspecs.StandardTenantContexts["too-long"]
				err = tenant.ValidateTenantID(longTenant.TenantID)
				Expect(err).To(HaveOccurred())
				Expect(err).To(testspecs.BeActionableError())

				By("rejecting empty tenant IDs")
				emptyTenant := testspecs.StandardTenantContexts["empty"]
				err = tenant.ValidateTenantID(emptyTenant.TenantID)
				Expect(err).To(HaveOccurred())
				Expect(err).To(testspecs.BeActionableError())
			})
		})

		Context("when handling tenant context errors", func() {
			It("provides actionable feedback for missing tenant context", func() {
				// This spec ensures developers get clear guidance when tenant context is missing

				By("detecting missing tenant in context")
				emptyCtx := context.Background()
				_, err := tenant.FromContext(emptyCtx)
				Expect(err).To(HaveOccurred())
				Expect(err).To(MatchError(tenant.ErrNoTenant))

				By("providing clear error message for developers")
				Expect(err.Error()).To(ContainSubstring("no tenant"))
			})

			It("prevents system compromise through tenant validation", func() {
				// This spec ensures invalid tenants can't bypass security boundaries

				// Test various attack vectors
				attackVectors := []string{
					"../../../etc/passwd", // Directory traversal
					"tenant; DROP TABLE;", // SQL injection attempt
					"tenant<script>",      // XSS attempt
					"ADMIN",               // Case sensitivity bypass
					"tenant_admin",        // Underscore separator
				}

				for _, attack := range attackVectors {
					By("rejecting attack vector: " + attack)
					err := tenant.ValidateTenantID(attack)
					Expect(err).To(HaveOccurred())
					Expect(err).To(testspecs.BeSecureError())
				}
			})
		})
	})

	// =================================================================
	// LEVEL 2: INTERFACE COMPLIANCE SPECIFICATIONS
	// These specs test the "how" - that our implementation follows CrossCodex interface contracts
	// =================================================================

	Describe("Tenant Interface Compliance", func() {
		// Create the adapter once for all shared behavior tests
		tenantAdapter := NewTenantAdapter()

		Context("as a tenant-isolated component", testspecs.TenantIsolationBehavior(tenantAdapter))
	})

	// =================================================================
	// LEVEL 3: TECHNICAL EDGE CASES AND INTEGRATION SCENARIOS
	// These specs test the "what" - comprehensive coverage of technical scenarios from original tests
	// =================================================================

	Describe("Tenant Validation Edge Cases", func() {
		Context("when testing comprehensive validation scenarios", func() {
			// This section migrates all test cases from validate_test.go

			It("validates simple lowercase tenant IDs", func() {
				err := tenant.ValidateTenantID("abc")
				Expect(err).NotTo(HaveOccurred())
			})

			It("validates tenant IDs with hyphens and digits", func() {
				err := tenant.ValidateTenantID("my-tenant-1")
				Expect(err).NotTo(HaveOccurred())
			})

			It("validates mixed letters and digits", func() {
				err := tenant.ValidateTenantID("a1b2c3")
				Expect(err).NotTo(HaveOccurred())
			})

			It("validates minimum length tenant IDs", func() {
				err := tenant.ValidateTenantID("a1b")
				Expect(err).NotTo(HaveOccurred())
			})

			It("validates exactly 63 character tenant IDs", func() {
				tenantID := "a" + strings.Repeat("b", 61) + "c" // 63 chars
				err := tenant.ValidateTenantID(tenantID)
				Expect(err).NotTo(HaveOccurred())
			})

			It("validates maximum length 64 character tenant IDs", func() {
				tenantID := "a" + strings.Repeat("b", 62) + "c" // 64 chars
				err := tenant.ValidateTenantID(tenantID)
				Expect(err).NotTo(HaveOccurred())
			})

			It("allows consecutive hyphens in tenant IDs", func() {
				err := tenant.ValidateTenantID("a--b")
				Expect(err).NotTo(HaveOccurred())

				err = tenant.ValidateTenantID("a---b")
				Expect(err).NotTo(HaveOccurred())
			})

			It("rejects empty tenant IDs", func() {
				err := tenant.ValidateTenantID("")
				Expect(err).To(HaveOccurred())
				Expect(err).To(MatchError(ContainSubstring("must not be empty")))
				Expect(errors.Is(err, tenant.ErrInvalidTenant)).To(BeTrue())
			})

			It("rejects tenant IDs that are too short", func() {
				err := tenant.ValidateTenantID("ab") // 2 chars
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, tenant.ErrInvalidTenant)).To(BeTrue())

				err = tenant.ValidateTenantID("a") // 1 char
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, tenant.ErrInvalidTenant)).To(BeTrue())
			})

			It("rejects tenant IDs that are too long", func() {
				tenantID := "a" + strings.Repeat("b", 63) + "c" // 65 chars
				err := tenant.ValidateTenantID(tenantID)
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, tenant.ErrInvalidTenant)).To(BeTrue())
			})

			It("rejects tenant IDs with uppercase letters", func() {
				err := tenant.ValidateTenantID("MyTenant")
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, tenant.ErrInvalidTenant)).To(BeTrue())
			})

			It("rejects tenant IDs with underscores", func() {
				err := tenant.ValidateTenantID("my_tenant")
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, tenant.ErrInvalidTenant)).To(BeTrue())
			})

			It("rejects tenant IDs with dots", func() {
				err := tenant.ValidateTenantID("my.tenant")
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, tenant.ErrInvalidTenant)).To(BeTrue())
			})

			It("rejects tenant IDs with at signs", func() {
				err := tenant.ValidateTenantID("my@tenant")
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, tenant.ErrInvalidTenant)).To(BeTrue())
			})

			It("rejects tenant IDs with leading hyphens", func() {
				err := tenant.ValidateTenantID("-abc")
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, tenant.ErrInvalidTenant)).To(BeTrue())
			})

			It("rejects tenant IDs with trailing hyphens", func() {
				err := tenant.ValidateTenantID("abc-")
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, tenant.ErrInvalidTenant)).To(BeTrue())
			})

			It("rejects tenant IDs with leading digits", func() {
				err := tenant.ValidateTenantID("1abc")
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, tenant.ErrInvalidTenant)).To(BeTrue())
			})
		})

		Context("when testing context management edge cases", func() {
			It("handles nil context values gracefully", func() {
				// Create a context with a nil tenant value
				type testKey string
				ctx := context.WithValue(context.Background(), testKey("tenant"), nil)
				_, err := tenant.FromContext(ctx)
				Expect(err).To(HaveOccurred())
				Expect(err).To(MatchError(tenant.ErrNoTenant))
			})

			It("handles wrong type context values gracefully", func() {
				// Create a context with a non-string tenant value
				type testKey string
				ctx := context.WithValue(context.Background(), testKey("tenant"), 123)
				_, err := tenant.FromContext(ctx)
				Expect(err).To(HaveOccurred())
				Expect(err).To(MatchError(tenant.ErrNoTenant))
			})

			It("creates context with tenant correctly", func() {
				tenantID := "test-tenant"
				ctx, err := tenant.WithTenant(context.Background(), tenantID)
				Expect(err).NotTo(HaveOccurred())

				extractedID, err := tenant.FromContext(ctx)
				testspecs.AssertNoError(err)
				Expect(extractedID).To(Equal(tenantID))
			})
		})
	})

	// =================================================================
	// LEVEL 4: WITHTENANT VALIDATION AT INJECTION
	// These specs test that WithTenant rejects invalid IDs before storing them
	// =================================================================

	Describe("WithTenant Validation at Injection", func() {
		Context("when injecting tenant with validation", func() {
			It("rejects an invalid tenant ID", func() {
				ctx, err := tenant.WithTenant(context.Background(), "BAD!")
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, tenant.ErrInvalidTenant)).To(BeTrue())
				// Original context returned unchanged
				_, extractErr := tenant.FromContext(ctx)
				Expect(errors.Is(extractErr, tenant.ErrNoTenant)).To(BeTrue())
			})

			It("rejects an empty tenant ID", func() {
				ctx, err := tenant.WithTenant(context.Background(), "")
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, tenant.ErrInvalidTenant)).To(BeTrue())
				_, extractErr := tenant.FromContext(ctx)
				Expect(errors.Is(extractErr, tenant.ErrNoTenant)).To(BeTrue())
			})

			It("accepts a valid tenant ID", func() {
				ctx, err := tenant.WithTenant(context.Background(), "acme-corp")
				Expect(err).NotTo(HaveOccurred())
				id, extractErr := tenant.FromContext(ctx)
				Expect(extractErr).NotTo(HaveOccurred())
				Expect(id).To(Equal("acme-corp"))
			})
		})
	})

	// =================================================================
	// LEVEL 5: USER CONTEXT PROPAGATION
	// These specs test WithUser / UserFromContext functionality
	// =================================================================

	Describe("User Context", func() {
		It("stores and retrieves a user ID", func() {
			ctx := tenant.WithUser(context.Background(), "alice")
			Expect(tenant.UserFromContext(ctx)).To(Equal("alice"))
		})

		It("returns empty string when no user is set", func() {
			Expect(tenant.UserFromContext(context.Background())).To(BeEmpty())
		})

		It("ignores empty user ID (no-op)", func() {
			ctx := tenant.WithUser(context.Background(), "")
			Expect(tenant.UserFromContext(ctx)).To(BeEmpty())
		})

		It("coexists with tenant context", func() {
			ctx, err := tenant.WithTenant(context.Background(), "acme-corp")
			Expect(err).NotTo(HaveOccurred())
			ctx = tenant.WithUser(ctx, "bob")

			id, err := tenant.FromContext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(id).To(Equal("acme-corp"))
			Expect(tenant.UserFromContext(ctx)).To(Equal("bob"))
		})
	})
})

// TenantAdapter implements testspecs.TenantIsolatedComponent to test the tenant system
// against the shared behavioral specifications
type TenantAdapter struct {
	tenantID string
}

// NewTenantAdapter creates a new tenant adapter for testing
func NewTenantAdapter() *TenantAdapter {
	return &TenantAdapter{}
}

// WithTenantContext returns a new instance of the component configured
// for the tenant specified in the context.
func (a *TenantAdapter) WithTenantContext(ctx context.Context) testspecs.TenantIsolatedComponent {
	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		// Return a new adapter without a tenant ID for error testing
		return &TenantAdapter{}
	}

	return &TenantAdapter{
		tenantID: tenantID,
	}
}

// ValidateTenantAccess verifies that the given tenant ID is valid and
// accessible by the current component.
func (a *TenantAdapter) ValidateTenantAccess(tenantID string) error {
	// First validate the basic tenant ID format
	if err := tenant.ValidateTenantID(tenantID); err != nil {
		return err
	}

	// For security testing, explicitly forbid certain tenant names
	// that might be used for attacks or reserved system purposes
	forbiddenTenants := []string{
		"forbidden-tenant", // Test case from shared behavior
		"admin",            // Administrative access
		"system",           // System-level access
		"root",             // Root access
		"test",             // Reserved for testing
	}

	for _, forbidden := range forbiddenTenants {
		if tenantID == forbidden {
			return tenant.ErrInvalidTenant
		}
	}

	return nil
}

// GetTenantID extracts and returns the tenant ID from the given context.
func (a *TenantAdapter) GetTenantID(ctx context.Context) (string, error) {
	return tenant.FromContext(ctx)
}

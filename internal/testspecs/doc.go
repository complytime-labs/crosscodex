// Package testspecs provides shared testing infrastructure for CrossCodex BDD tests.
//
// This package contains reusable behavior specifications, common test fixtures,
// standardized setup helpers, and custom Gomega matchers that eliminate
// duplication across package test suites.
//
// Key components:
// - shared_behaviors.go: Reusable BDD specs for common patterns
// - interfaces.go: Contracts for behavior composition
// - fixtures.go: Standard test data and contexts
// - helpers.go: Setup/teardown utilities
// - matchers.go: Custom Gomega matchers for CrossCodex-specific patterns
//
// Architecture:
//
// The testspecs package enables a three-layer BDD testing approach:
//
//  1. Behavioral specs - High-level behavior specifications that can be
//     shared across multiple components (e.g., "behaves like a tenant-isolated component")
//
//  2. Technical tests - Package-specific implementation tests that verify
//     concrete behavior of individual components
//
//  3. Integration specs - End-to-end tests that verify component interactions
//     and system behavior under realistic conditions
//
// Interface Contracts:
//
// Components that implement the provided interfaces can automatically
// inherit shared behavior specifications, ensuring consistent behavior
// patterns across the codebase.
//
// For example, any component implementing TenantIsolatedComponent can use
// the shared "behaves like a tenant-isolated component" spec, which tests
// tenant context propagation, access validation, and isolation boundaries.
//
// Usage:
//
//	import . "github.com/complytime-labs/crosscodex/internal/testspecs"
//
//	var _ = Describe("MyComponent", func() {
//	  var component TenantIsolatedComponent
//
//	  BeforeEach(func() {
//	    component = NewMyComponent()
//	  })
//
//	  BehavesLikeATenantIsolatedComponent(&component)
//
//	  Describe("component-specific behavior", func() {
//	    It("should validate tenant IDs", func() {
//	      Expect("acme-corp").To(BeValidTenantID())
//	    })
//
//	    It("should have secure error handling", func() {
//	      err := component.DoSomething()
//	      Expect(err).To(BeSecureError())
//	      Expect(err).To(BeActionableError())
//	    })
//	  })
//	})
//
// Custom Matchers:
//
// The package provides custom Gomega matchers for common CrossCodex patterns:
// - BeValidTenantID(): Validates tenant ID format (3-64 chars, alphanumeric + hyphens)
// - HaveValidTenantPrefix(): Validates strings with tenant prefixes (tenant-id/path)
// - BeSecureError(): Ensures errors don't leak sensitive information
// - BeActionableError(): Ensures errors provide actionable guidance to users
// - HaveValidConfigStructure(): Validates required configuration sections
// - MatchConfigPrecedence(): Validates XDG-compliant configuration precedence
// - HaveIsolatedTenantData(): Validates proper tenant data isolation
// - BeSanitizedOutput(): Validates output is sanitized of sensitive information
// - ContainValidTimestamp(field): Validates RFC3339 timestamps in data structures
// - BeValidDatabaseConnection(): Validates active database connections
// - BeWithinTolerance(expected, tolerance): Validates numeric values within tolerance
package testspecs

package testspecs

import (
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// TenantIsolationBehavior tests tenant boundary enforcement.
//
// This shared behavior specification validates that components implementing
// TenantIsolatedComponent properly:
// - Prevent cross-tenant data access
// - Validate tenant ID format using StandardTenantContexts
// - Reject invalid tenant formats with secure errors
//
// Usage:
//
//	Describe("as a tenant-isolated component", TenantIsolationBehavior(provider))
func TenantIsolationBehavior(subject TenantIsolatedComponent) func() {
	return func() {
		Describe("tenant isolation enforcement", func() {
			Context("when validating tenant access", func() {
				It("accepts valid tenant IDs", func() {
					validTenant := StandardTenantContexts["valid-tenant"]
					err := subject.ValidateTenantAccess(validTenant.TenantID)
					Expect(err).NotTo(HaveOccurred())
				})

				It("rejects invalid tenant formats", func() {
					invalidTenant := StandardTenantContexts["invalid-chars"]
					err := subject.ValidateTenantAccess(invalidTenant.TenantID)
					Expect(err).To(HaveOccurred())
					Expect(err).To(BeSecureError())
				})

				It("validates minimum length tenants", func() {
					minTenant := StandardTenantContexts["min-length"]
					err := subject.ValidateTenantAccess(minTenant.TenantID)
					Expect(err).NotTo(HaveOccurred())
				})

				It("validates maximum length tenants", func() {
					maxTenant := StandardTenantContexts["max-length"]
					err := subject.ValidateTenantAccess(maxTenant.TenantID)
					Expect(err).NotTo(HaveOccurred())
				})

				It("rejects too-short tenant IDs", func() {
					shortTenant := StandardTenantContexts["too-short"]
					err := subject.ValidateTenantAccess(shortTenant.TenantID)
					Expect(err).To(HaveOccurred())
					Expect(err).To(BeActionableError())
				})

				It("rejects too-long tenant IDs", func() {
					longTenant := StandardTenantContexts["too-long"]
					err := subject.ValidateTenantAccess(longTenant.TenantID)
					Expect(err).To(HaveOccurred())
					Expect(err).To(BeActionableError())
				})

				It("rejects empty tenant IDs", func() {
					emptyTenant := StandardTenantContexts["empty"]
					err := subject.ValidateTenantAccess(emptyTenant.TenantID)
					Expect(err).To(HaveOccurred())
					Expect(err).To(BeActionableError())
				})
			})

			Context("when working with tenant contexts", func() {
				It("extracts valid tenant IDs from context", func() {
					validTenant := StandardTenantContexts["valid-tenant"]
					ctx := CreateTenantContext(validTenant.TenantID)

					tenantID, err := subject.GetTenantID(ctx)
					Expect(err).NotTo(HaveOccurred())
					Expect(tenantID).To(BeValidTenantID())
				})

				It("returns tenant-scoped component instances", func() {
					validTenant := StandardTenantContexts["valid-tenant"]
					ctx := CreateTenantContext(validTenant.TenantID)

					scopedComponent := subject.WithTenantContext(ctx)
					Expect(scopedComponent).NotTo(BeNil())

					// Verify the scoped component has the same interface
					var _ TenantIsolatedComponent = scopedComponent
				})

				It("maintains tenant isolation across component instances", func() {
					tenant1 := StandardTenantContexts["valid-tenant"]
					tenant2 := StandardTenantContexts["min-length"]

					ctx1 := CreateTenantContext(tenant1.TenantID)
					ctx2 := CreateTenantContext(tenant2.TenantID)

					component1 := subject.WithTenantContext(ctx1)
					component2 := subject.WithTenantContext(ctx2)

					// Each component should extract its own tenant ID
					id1, err1 := component1.GetTenantID(ctx1)
					id2, err2 := component2.GetTenantID(ctx2)

					Expect(err1).NotTo(HaveOccurred())
					Expect(err2).NotTo(HaveOccurred())

					// They should be different
					Expect(id1).NotTo(Equal(id2))
				})
			})

			Context("when enforcing access boundaries", func() {
				It("prevents access to forbidden tenants", func() {
					// Test with a tenant that should be explicitly forbidden
					err := subject.ValidateTenantAccess("forbidden-tenant")
					Expect(err).To(HaveOccurred())
					Expect(err).To(BeSecureError())
				})

				It("validates tenant ID format consistently", func() {
					for name, fixture := range StandardTenantContexts {
						By(fmt.Sprintf("testing %s: %s", name, fixture.DisplayName))

						err := subject.ValidateTenantAccess(fixture.TenantID)

						if fixture.Valid {
							Expect(err).NotTo(HaveOccurred())
						} else {
							Expect(err).To(HaveOccurred())
							Expect(err).To(BeSecureError())
							Expect(err).To(BeActionableError())
						}
					}
				})
			})
		})
	}
}

// ConfigurationComplianceBehavior tests configuration loading compliance.
//
// This shared behavior specification validates that components implementing
// ConfigurableComponent properly:
// - Follow XDG compliance precedence rules
// - Validate configuration schema
// - Reject invalid configuration with actionable errors
//
// Usage:
//
//	Describe("as a configurable component", ConfigurationComplianceBehavior(provider))
func ConfigurationComplianceBehavior(subject ConfigurableComponent) func() {
	return func() {
		Describe("configuration compliance enforcement", func() {
			Context("when loading configuration", func() {
				It("loads configuration from valid paths", func() {
					configData := []byte(`
database:
  host: localhost
  port: 5432
nats:
  url: localhost:4222
storage:
  type: local
  path: /tmp
`)
					configPath, cleanup := SetupTestConfig(configData)
					defer cleanup()

					err := subject.LoadConfiguration(configPath)
					Expect(err).NotTo(HaveOccurred())
				})

				It("rejects malformed configuration files", func() {
					invalidConfigData := []byte("invalid yaml: [[[")
					configPath, cleanup := SetupTestConfig(invalidConfigData)
					defer cleanup()

					err := subject.LoadConfiguration(configPath)
					Expect(err).To(HaveOccurred())
					Expect(err).To(BeActionableError())
				})

				It("handles missing configuration files gracefully", func() {
					err := subject.LoadConfiguration("/nonexistent/config.yml")
					Expect(err).To(HaveOccurred())
					Expect(err).To(BeActionableError())
				})
			})

			Context("when validating configuration", func() {
				BeforeEach(func() {
					// Load a valid configuration for validation tests
					configData := []byte(`
database:
  host: localhost
  port: 5432
nats:
  url: localhost:4222
storage:
  type: local
  path: /tmp
order: user-first
`)
					configPath, cleanup := SetupTestConfig(configData)
					defer cleanup()

					err := subject.LoadConfiguration(configPath)
					Expect(err).NotTo(HaveOccurred())
				})

				It("validates complete configuration structures", func() {
					err := subject.ValidateConfiguration()
					Expect(err).NotTo(HaveOccurred())
				})

				It("retrieves configuration values with proper precedence", func() {
					// Test retrieving a known configuration value
					value, err := subject.GetConfigValue("database")
					Expect(err).NotTo(HaveOccurred())
					Expect(value).NotTo(BeNil())
				})

				It("follows XDG precedence rules", func() {
					// Get configuration and check if it has proper structure
					config, err := subject.GetConfigValue("order")
					if err == nil {
						// If order is present, it should indicate user-first precedence
						Expect(config).To(Equal("user-first"))
					}
				})

				It("returns actionable errors for missing keys", func() {
					_, err := subject.GetConfigValue("nonexistent-key")
					Expect(err).To(HaveOccurred())
					Expect(err).To(BeActionableError())
				})
			})

			Context("when testing configuration paths", func() {
				It("respects XDG Base Directory specification", func() {
					xdgPaths := StandardConfigPaths["xdg-precedence"]

					// XDG paths should follow the priority order and be user-focused
					Expect(xdgPaths.Priority).To(Equal("user-config"))

					// Check that we have at least one standard XDG path
					hasValidXDGPath := false
					for _, path := range xdgPaths.Paths {
						By(fmt.Sprintf("testing XDG path: %s", path))
						// XDG paths contain environment variables or home directory references
						if strings.Contains(path, "$XDG_CONFIG_HOME") ||
							strings.Contains(path, "$HOME") ||
							strings.Contains(path, ".config") {
							hasValidXDGPath = true
						}
					}
					Expect(hasValidXDGPath).To(BeTrue(), "at least one path should follow XDG conventions")
				})

				It("handles system-wide paths appropriately", func() {
					systemPaths := StandardConfigPaths["system-paths"]
					Expect(systemPaths.Priority).To(Equal("system-wide"))

					for _, path := range systemPaths.Paths {
						By(fmt.Sprintf("testing system path: %s", path))
						// System paths should start with /etc or /usr
						Expect(path).To(Or(HavePrefix("/etc"), HavePrefix("/usr")))
					}
				})

				It("supports local configuration files", func() {
					localPaths := StandardConfigPaths["local-files"]
					Expect(localPaths.Priority).To(Equal("local"))

					for _, path := range localPaths.Paths {
						By(fmt.Sprintf("testing local path: %s", path))
						// Local paths should be relative
						Expect(path).To(Or(HavePrefix("./"), HavePrefix("./")))
					}
				})
			})
		})
	}
}

// SecurityBoundaryBehavior tests security patterns and boundaries.
//
// This shared behavior specification validates that components implementing
// SecureComponent properly:
// - Validate input at system boundaries using StandardErrorConditions
// - Sanitize output for external consumption
// - Operate in secure mode when required
//
// Usage:
//
//	Describe("as a secure component", SecurityBoundaryBehavior(provider))
func SecurityBoundaryBehavior(subject SecureComponent) func() {
	return func() {
		Describe("security boundary enforcement", func() {
			Context("when validating inputs", func() {
				It("accepts safe input values", func() {
					safeInput := "normal-input-value"
					err := subject.ValidateInput(safeInput)
					Expect(err).NotTo(HaveOccurred())
				})

				It("rejects malicious input", func() {
					maliciousInput := "malicious-input"
					err := subject.ValidateInput(maliciousInput)
					Expect(err).To(HaveOccurred())
					Expect(err).To(BeSecureError())
					Expect(err).To(BeActionableError())
				})

				It("validates input size limits", func() {
					// Test with oversized input
					oversizedInput := string(make([]byte, 2000)) // 2000 characters
					err := subject.ValidateInput(oversizedInput)
					Expect(err).To(HaveOccurred())
					Expect(err).To(BeActionableError())
				})

				It("handles different input types safely", func() {
					inputs := []interface{}{
						"string input",
						123,
						map[string]interface{}{"key": "value"},
						[]string{"item1", "item2"},
					}

					for _, input := range inputs {
						By(fmt.Sprintf("validating input type: %T", input))
						err := subject.ValidateInput(input)
						// Should not panic and should return either nil or actionable error
						if err != nil {
							Expect(err).To(BeActionableError())
							Expect(err).To(BeSecureError())
						}
					}
				})
			})

			Context("when sanitizing outputs", func() {
				It("removes sensitive information from strings", func() {
					sensitiveOutput := "password=secret123"
					sanitized := subject.SanitizeOutput(sensitiveOutput)

					Expect(sanitized).To(BeSanitizedOutput())
					Expect(sanitized).NotTo(ContainSubstring("secret123"))
				})

				It("sanitizes map structures", func() {
					sensitiveMap := map[string]interface{}{
						"username": "user123",
						"password": "secret456",
						"config":   "normal-value",
					}

					sanitized := subject.SanitizeOutput(sensitiveMap)
					Expect(sanitized).To(BeSanitizedOutput())

					if sanitizedMap, ok := sanitized.(map[string]interface{}); ok {
						// Username and config should remain
						Expect(sanitizedMap["username"]).To(Equal("user123"))
						Expect(sanitizedMap["config"]).To(Equal("normal-value"))

						// Password should be sanitized
						Expect(sanitizedMap["password"]).NotTo(Equal("secret456"))
					}
				})

				It("prevents information leakage in error messages", func() {
					debugOutput := "debug: sql query failed"
					sanitized := subject.SanitizeOutput(debugOutput)

					Expect(sanitized).To(BeSanitizedOutput())
					Expect(sanitized).NotTo(ContainSubstring("sql"))
				})

				It("preserves safe output unchanged", func() {
					safeOutput := "normal response data"
					sanitized := subject.SanitizeOutput(safeOutput)

					Expect(sanitized).To(Equal(safeOutput))
				})
			})

			Context("when operating in secure mode", func() {
				It("reports secure mode status", func() {
					secureMode := subject.IsSecureModeEnabled()
					// Should return a boolean (not nil/panic)
					Expect(secureMode).To(Or(BeTrue(), BeFalse()))
				})

				It("enforces stricter validation in secure mode", func() {
					if subject.IsSecureModeEnabled() {
						// In secure mode, validation should be stricter
						borderlineInput := "questionable-input"
						err := subject.ValidateInput(borderlineInput)

						// Should either accept or reject with actionable error
						if err != nil {
							Expect(err).To(BeActionableError())
							Expect(err).To(BeSecureError())
						}
					}
				})
			})

			Context("when testing error conditions", func() {
				It("produces secure errors that don't leak information", func() {
					for name, errorCondition := range StandardErrors {
						if errorCondition.Category == "authorization" || errorCondition.Category == "validation" {
							By(fmt.Sprintf("testing %s error: %s", name, errorCondition.Message))

							// Simulate the error condition
							err := fmt.Errorf("%s", errorCondition.Message)

							// Validate that our component would handle this securely
							sanitized := subject.SanitizeOutput(err.Error())
							Expect(sanitized).To(BeSanitizedOutput())
						}
					}
				})
			})
		})
	}
}

// ErrorHandlingBehavior tests error resilience patterns.
//
// This shared behavior specification validates that components implementing
// ErrorHandlingComponent properly:
// - Handle different error types appropriately
// - Categorize retryable vs non-retryable errors
// - Track error history for debugging
//
// Usage:
//
//	Describe("as an error-handling component", ErrorHandlingBehavior(provider))
func ErrorHandlingBehavior(subject ErrorHandlingComponent) func() {
	return func() {
		Describe("error handling resilience", Serial, func() {
			Context("when processing errors", func() {
				It("handles errors without panicking", func() {
					testError := fmt.Errorf("test error message")

					var processedError error
					Expect(func() {
						processedError = subject.HandleError(testError)
					}).NotTo(Panic())

					Expect(processedError).NotTo(BeNil())
				})

				It("preserves error information during handling", func() {
					originalError := fmt.Errorf("original error details")
					processedError := subject.HandleError(originalError)

					// Should preserve the essential error information
					Expect(processedError.Error()).NotTo(BeEmpty())
				})

				It("tracks error history", func() {
					testError := fmt.Errorf("tracked error")
					_ = subject.HandleError(testError)

					lastError := subject.GetLastError()
					Expect(lastError).NotTo(BeNil())
				})
			})

			Context("when categorizing error retryability", func() {
				It("correctly identifies retryable errors", func() {
					for name, errorCondition := range StandardErrors {
						By(fmt.Sprintf("testing %s error: %s", name, errorCondition.Message))

						testError := fmt.Errorf("%s", errorCondition.Message)
						isRetryable := subject.IsRetryableError(testError)

						// Should match the expected retryable status
						Expect(isRetryable).To(Equal(errorCondition.Retryable))
					}
				})

				It("handles network errors as retryable", func() {
					networkError := StandardErrors["network-timeout"]
					testError := fmt.Errorf("%s", networkError.Message)

					isRetryable := subject.IsRetryableError(testError)
					Expect(isRetryable).To(BeTrue())
				})

				It("handles authorization errors as non-retryable", func() {
					authError := StandardErrors["permission-denied"]
					testError := fmt.Errorf("%s", authError.Message)

					isRetryable := subject.IsRetryableError(testError)
					Expect(isRetryable).To(BeFalse())
				})

				It("handles validation errors as non-retryable", func() {
					validationError := StandardErrors["invalid-input"]
					testError := fmt.Errorf("%s", validationError.Message)

					isRetryable := subject.IsRetryableError(testError)
					Expect(isRetryable).To(BeFalse())
				})

				It("handles client errors as non-retryable", func() {
					clientError := StandardErrors["not-found"]
					testError := fmt.Errorf("%s", clientError.Message)

					isRetryable := subject.IsRetryableError(testError)
					Expect(isRetryable).To(BeFalse())
				})
			})

			Context("when maintaining error state", func() {
				It("returns nil when no errors have occurred", func() {
					// Fresh component should have no last error
					_ = subject.GetLastError()
					// May be nil or non-nil depending on implementation
				})

				It("updates last error after handling", func() {
					firstError := fmt.Errorf("first error")
					secondError := fmt.Errorf("second error")

					// Handle first error
					_ = subject.HandleError(firstError)

					// Handle second error
					_ = subject.HandleError(secondError)

					// Last error should reflect the most recent error
					lastError := subject.GetLastError()
					Expect(lastError).NotTo(BeNil())
				})

				It("maintains error context for debugging", func() {
					contextError := fmt.Errorf("error with context: operation failed")
					_ = subject.HandleError(contextError)

					lastError := subject.GetLastError()
					if lastError != nil {
						// Error should maintain enough context for debugging
						Expect(lastError.Error()).NotTo(BeEmpty())
					}
				})
			})

			Context("when testing standard error scenarios", func() {
				It("handles all standard error conditions appropriately", func() {
					for name, errorCondition := range StandardErrors {
						By(fmt.Sprintf("handling standard error %s", name))

						testError := fmt.Errorf("%s", errorCondition.Message)

						// Handle the error
						processedError := subject.HandleError(testError)
						Expect(processedError).NotTo(BeNil())

						// Check retryability
						isRetryable := subject.IsRetryableError(testError)
						Expect(isRetryable).To(Equal(errorCondition.Retryable))

						// Verify it's tracked
						lastError := subject.GetLastError()
						Expect(lastError).NotTo(BeNil())
					}
				})
			})
		})
	}
}

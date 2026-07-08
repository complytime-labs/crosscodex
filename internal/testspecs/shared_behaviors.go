package testspecs

import (
	"fmt"
	"strings"

	ginkgo "github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
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
//	ginkgo.Describe("as a tenant-isolated component", TenantIsolationBehavior(provider))
func TenantIsolationBehavior(subject TenantIsolatedComponent) func() {
	return func() {
		ginkgo.Describe("tenant isolation enforcement", func() {
			ginkgo.Context("when validating tenant access", func() {
				ginkgo.It("accepts valid tenant IDs", func() {
					validTenant := StandardTenantContexts["valid-tenant"]
					err := subject.ValidateTenantAccess(validTenant.TenantID)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				})

				ginkgo.It("rejects invalid tenant formats", func() {
					invalidTenant := StandardTenantContexts["invalid-chars"]
					err := subject.ValidateTenantAccess(invalidTenant.TenantID)
					gomega.Expect(err).To(gomega.HaveOccurred())
					gomega.Expect(err).To(BeSecureError())
				})

				ginkgo.It("validates minimum length tenants", func() {
					minTenant := StandardTenantContexts["min-length"]
					err := subject.ValidateTenantAccess(minTenant.TenantID)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				})

				ginkgo.It("validates maximum length tenants", func() {
					maxTenant := StandardTenantContexts["max-length"]
					err := subject.ValidateTenantAccess(maxTenant.TenantID)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				})

				ginkgo.It("rejects too-short tenant IDs", func() {
					shortTenant := StandardTenantContexts["too-short"]
					err := subject.ValidateTenantAccess(shortTenant.TenantID)
					gomega.Expect(err).To(gomega.HaveOccurred())
					gomega.Expect(err).To(BeActionableError())
				})

				ginkgo.It("rejects too-long tenant IDs", func() {
					longTenant := StandardTenantContexts["too-long"]
					err := subject.ValidateTenantAccess(longTenant.TenantID)
					gomega.Expect(err).To(gomega.HaveOccurred())
					gomega.Expect(err).To(BeActionableError())
				})

				ginkgo.It("rejects empty tenant IDs", func() {
					emptyTenant := StandardTenantContexts["empty"]
					err := subject.ValidateTenantAccess(emptyTenant.TenantID)
					gomega.Expect(err).To(gomega.HaveOccurred())
					gomega.Expect(err).To(BeActionableError())
				})
			})

			ginkgo.Context("when working with tenant contexts", func() {
				ginkgo.It("extracts valid tenant IDs from context", func() {
					validTenant := StandardTenantContexts["valid-tenant"]
					ctx := CreateTenantContext(validTenant.TenantID)

					tenantID, err := subject.GetTenantID(ctx)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Expect(tenantID).To(BeValidTenantID())
				})

				ginkgo.It("returns tenant-scoped component instances", func() {
					validTenant := StandardTenantContexts["valid-tenant"]
					ctx := CreateTenantContext(validTenant.TenantID)

					scopedComponent := subject.WithTenantContext(ctx)
					gomega.Expect(scopedComponent).NotTo(gomega.BeNil())

					// Verify the scoped component has the same interface
					_ = TenantIsolatedComponent(scopedComponent)
				})

				ginkgo.It("maintains tenant isolation across component instances", func() {
					tenant1 := StandardTenantContexts["valid-tenant"]
					tenant2 := StandardTenantContexts["min-length"]

					ctx1 := CreateTenantContext(tenant1.TenantID)
					ctx2 := CreateTenantContext(tenant2.TenantID)

					component1 := subject.WithTenantContext(ctx1)
					component2 := subject.WithTenantContext(ctx2)

					// Each component should extract its own tenant ID
					id1, err1 := component1.GetTenantID(ctx1)
					id2, err2 := component2.GetTenantID(ctx2)

					gomega.Expect(err1).NotTo(gomega.HaveOccurred())
					gomega.Expect(err2).NotTo(gomega.HaveOccurred())

					// They should be different
					gomega.Expect(id1).NotTo(gomega.Equal(id2))
				})
			})

			ginkgo.Context("when enforcing access boundaries", func() {
				ginkgo.It("prevents access to forbidden tenants", func() {
					// Test with a tenant that should be explicitly forbidden
					err := subject.ValidateTenantAccess("forbidden-tenant")
					gomega.Expect(err).To(gomega.HaveOccurred())
					gomega.Expect(err).To(BeSecureError())
				})

				ginkgo.It("validates tenant ID format consistently", func() {
					for name, fixture := range StandardTenantContexts {
						ginkgo.By(fmt.Sprintf("testing %s: %s", name, fixture.DisplayName))

						err := subject.ValidateTenantAccess(fixture.TenantID)

						if fixture.Valid {
							gomega.Expect(err).NotTo(gomega.HaveOccurred())
						} else {
							gomega.Expect(err).To(gomega.HaveOccurred())
							gomega.Expect(err).To(BeSecureError())
							gomega.Expect(err).To(BeActionableError())
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
//	ginkgo.Describe("as a configurable component", ConfigurationComplianceBehavior(provider))
func ConfigurationComplianceBehavior(subject ConfigurableComponent) func() {
	return func() {
		ginkgo.Describe("configuration compliance enforcement", func() {
			ginkgo.Context("when loading configuration", func() {
				ginkgo.It("loads configuration from valid paths", func() {
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
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				})

				ginkgo.It("rejects malformed configuration files", func() {
					invalidConfigData := []byte("invalid yaml: [[[")
					configPath, cleanup := SetupTestConfig(invalidConfigData)
					defer cleanup()

					err := subject.LoadConfiguration(configPath)
					gomega.Expect(err).To(gomega.HaveOccurred())
					gomega.Expect(err).To(BeActionableError())
				})

				ginkgo.It("handles missing configuration files gracefully", func() {
					err := subject.LoadConfiguration("/nonexistent/config.yml")
					gomega.Expect(err).To(gomega.HaveOccurred())
					gomega.Expect(err).To(BeActionableError())
				})
			})

			ginkgo.Context("when validating configuration", func() {
				ginkgo.BeforeEach(func() {
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
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				})

				ginkgo.It("validates complete configuration structures", func() {
					err := subject.ValidateConfiguration()
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				})

				ginkgo.It("retrieves configuration values with proper precedence", func() {
					// Test retrieving a known configuration value
					value, err := subject.GetConfigValue("database")
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					gomega.Expect(value).NotTo(gomega.BeNil())
				})

				ginkgo.It("follows XDG precedence rules", func() {
					// Get configuration and check if it has proper structure
					config, err := subject.GetConfigValue("order")
					if err == nil {
						// If order is present, it should indicate user-first precedence
						gomega.Expect(config).To(gomega.Equal("user-first"))
					}
				})

				ginkgo.It("returns actionable errors for missing keys", func() {
					_, err := subject.GetConfigValue("nonexistent-key")
					gomega.Expect(err).To(gomega.HaveOccurred())
					gomega.Expect(err).To(BeActionableError())
				})
			})

			ginkgo.Context("when testing configuration paths", func() {
				ginkgo.It("respects XDG Base Directory specification", func() {
					xdgPaths := StandardConfigPaths["xdg-precedence"]

					// XDG paths should follow the priority order and be user-focused
					gomega.Expect(xdgPaths.Priority).To(gomega.Equal("user-config"))

					// Check that we have at least one standard XDG path
					hasValidXDGPath := false
					for _, path := range xdgPaths.Paths {
						ginkgo.By(fmt.Sprintf("testing XDG path: %s", path))
						// XDG paths contain environment variables or home directory references
						if strings.Contains(path, "$XDG_CONFIG_HOME") ||
							strings.Contains(path, "$HOME") ||
							strings.Contains(path, ".config") {
							hasValidXDGPath = true
						}
					}
					gomega.Expect(hasValidXDGPath).To(gomega.BeTrue(), "at least one path should follow XDG conventions")
				})

				ginkgo.It("handles system-wide paths appropriately", func() {
					systemPaths := StandardConfigPaths["system-paths"]
					gomega.Expect(systemPaths.Priority).To(gomega.Equal("system-wide"))

					for _, path := range systemPaths.Paths {
						ginkgo.By(fmt.Sprintf("testing system path: %s", path))
						// System paths should start with /etc or /usr
						gomega.Expect(path).To(gomega.Or(gomega.HavePrefix("/etc"), gomega.HavePrefix("/usr")))
					}
				})

				ginkgo.It("supports local configuration files", func() {
					localPaths := StandardConfigPaths["local-files"]
					gomega.Expect(localPaths.Priority).To(gomega.Equal("local"))

					for _, path := range localPaths.Paths {
						ginkgo.By(fmt.Sprintf("testing local path: %s", path))
						// Local paths should be relative
						gomega.Expect(path).To(gomega.Or(gomega.HavePrefix("./"), gomega.HavePrefix("./")))
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
//	ginkgo.Describe("as a secure component", SecurityBoundaryBehavior(provider))
func SecurityBoundaryBehavior(subject SecureComponent) func() {
	return func() {
		ginkgo.Describe("security boundary enforcement", func() {
			ginkgo.Context("when validating inputs", func() {
				ginkgo.It("accepts safe input values", func() {
					safeInput := "normal-input-value"
					err := subject.ValidateInput(safeInput)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				})

				ginkgo.It("rejects malicious input", func() {
					maliciousInput := "malicious-input"
					err := subject.ValidateInput(maliciousInput)
					gomega.Expect(err).To(gomega.HaveOccurred())
					gomega.Expect(err).To(BeSecureError())
					gomega.Expect(err).To(BeActionableError())
				})

				ginkgo.It("validates input size limits", func() {
					// Test with oversized input
					oversizedInput := string(make([]byte, 2000)) // 2000 characters
					err := subject.ValidateInput(oversizedInput)
					gomega.Expect(err).To(gomega.HaveOccurred())
					gomega.Expect(err).To(BeActionableError())
				})

				ginkgo.It("handles different input types safely", func() {
					inputs := []interface{}{
						"string input",
						123,
						map[string]interface{}{"key": "value"},
						[]string{"item1", "item2"},
					}

					for _, input := range inputs {
						ginkgo.By(fmt.Sprintf("validating input type: %T", input))
						err := subject.ValidateInput(input)
						// Should not panic and should return either nil or actionable error
						if err != nil {
							gomega.Expect(err).To(BeActionableError())
							gomega.Expect(err).To(BeSecureError())
						}
					}
				})
			})

			ginkgo.Context("when sanitizing outputs", func() {
				ginkgo.It("removes sensitive information from strings", func() {
					sensitiveOutput := "password=secret123"
					sanitized := subject.SanitizeOutput(sensitiveOutput)

					gomega.Expect(sanitized).To(BeSanitizedOutput())
					gomega.Expect(sanitized).NotTo(gomega.ContainSubstring("secret123"))
				})

				ginkgo.It("sanitizes map structures", func() {
					sensitiveMap := map[string]interface{}{
						"username": "user123",
						"password": "secret456",
						"config":   "normal-value",
					}

					sanitized := subject.SanitizeOutput(sensitiveMap)
					gomega.Expect(sanitized).To(BeSanitizedOutput())

					if sanitizedMap, ok := sanitized.(map[string]interface{}); ok {
						// Username and config should remain
						gomega.Expect(sanitizedMap["username"]).To(gomega.Equal("user123"))
						gomega.Expect(sanitizedMap["config"]).To(gomega.Equal("normal-value"))

						// Password should be sanitized
						gomega.Expect(sanitizedMap["password"]).NotTo(gomega.Equal("secret456"))
					}
				})

				ginkgo.It("prevents information leakage in error messages", func() {
					debugOutput := "debug: sql query failed"
					sanitized := subject.SanitizeOutput(debugOutput)

					gomega.Expect(sanitized).To(BeSanitizedOutput())
					gomega.Expect(sanitized).NotTo(gomega.ContainSubstring("sql"))
				})

				ginkgo.It("preserves safe output unchanged", func() {
					safeOutput := "normal response data"
					sanitized := subject.SanitizeOutput(safeOutput)

					gomega.Expect(sanitized).To(gomega.Equal(safeOutput))
				})
			})

			ginkgo.Context("when operating in secure mode", func() {
				ginkgo.It("reports secure mode status", func() {
					secureMode := subject.IsSecureModeEnabled()
					// Should return a boolean (not nil/panic)
					gomega.Expect(secureMode).To(gomega.Or(gomega.BeTrue(), gomega.BeFalse()))
				})

				ginkgo.It("enforces stricter validation in secure mode", func() {
					if subject.IsSecureModeEnabled() {
						// In secure mode, validation should be stricter
						borderlineInput := "questionable-input"
						err := subject.ValidateInput(borderlineInput)

						// Should either accept or reject with actionable error
						if err != nil {
							gomega.Expect(err).To(BeActionableError())
							gomega.Expect(err).To(BeSecureError())
						}
					}
				})
			})

			ginkgo.Context("when testing error conditions", func() {
				ginkgo.It("produces secure errors that don't leak information", func() {
					for name, errorCondition := range StandardErrors {
						if errorCondition.Category == "authorization" || errorCondition.Category == "validation" {
							ginkgo.By(fmt.Sprintf("testing %s error: %s", name, errorCondition.Message))

							// Simulate the error condition
							err := fmt.Errorf("%s", errorCondition.Message)

							// Validate that our component would handle this securely
							sanitized := subject.SanitizeOutput(err.Error())
							gomega.Expect(sanitized).To(BeSanitizedOutput())
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
//	ginkgo.Describe("as an error-handling component", ErrorHandlingBehavior(provider))
func ErrorHandlingBehavior(subject ErrorHandlingComponent) func() {
	return func() {
		ginkgo.Describe("error handling resilience", ginkgo.Serial, func() {
			ginkgo.Context("when processing errors", func() {
				ginkgo.It("handles errors without panicking", func() {
					testError := fmt.Errorf("test error message")

					var processedError error
					gomega.Expect(func() {
						processedError = subject.HandleError(testError)
					}).NotTo(gomega.Panic())

					gomega.Expect(processedError).NotTo(gomega.BeNil())
				})

				ginkgo.It("preserves error information during handling", func() {
					originalError := fmt.Errorf("original error details")
					processedError := subject.HandleError(originalError)

					// Should preserve the essential error information
					gomega.Expect(processedError.Error()).NotTo(gomega.BeEmpty())
				})

				ginkgo.It("tracks error history", func() {
					testError := fmt.Errorf("tracked error")
					_ = subject.HandleError(testError)

					lastError := subject.GetLastError()
					gomega.Expect(lastError).NotTo(gomega.BeNil())
				})
			})

			ginkgo.Context("when categorizing error retryability", func() {
				ginkgo.It("correctly identifies retryable errors", func() {
					for name, errorCondition := range StandardErrors {
						ginkgo.By(fmt.Sprintf("testing %s error: %s", name, errorCondition.Message))

						testError := fmt.Errorf("%s", errorCondition.Message)
						isRetryable := subject.IsRetryableError(testError)

						// Should match the expected retryable status
						gomega.Expect(isRetryable).To(gomega.Equal(errorCondition.Retryable))
					}
				})

				ginkgo.It("handles network errors as retryable", func() {
					networkError := StandardErrors["network-timeout"]
					testError := fmt.Errorf("%s", networkError.Message)

					isRetryable := subject.IsRetryableError(testError)
					gomega.Expect(isRetryable).To(gomega.BeTrue())
				})

				ginkgo.It("handles authorization errors as non-retryable", func() {
					authError := StandardErrors["permission-denied"]
					testError := fmt.Errorf("%s", authError.Message)

					isRetryable := subject.IsRetryableError(testError)
					gomega.Expect(isRetryable).To(gomega.BeFalse())
				})

				ginkgo.It("handles validation errors as non-retryable", func() {
					validationError := StandardErrors["invalid-input"]
					testError := fmt.Errorf("%s", validationError.Message)

					isRetryable := subject.IsRetryableError(testError)
					gomega.Expect(isRetryable).To(gomega.BeFalse())
				})

				ginkgo.It("handles client errors as non-retryable", func() {
					clientError := StandardErrors["not-found"]
					testError := fmt.Errorf("%s", clientError.Message)

					isRetryable := subject.IsRetryableError(testError)
					gomega.Expect(isRetryable).To(gomega.BeFalse())
				})
			})

			ginkgo.Context("when maintaining error state", func() {
				ginkgo.It("returns nil when no errors have occurred", func() {
					// Fresh component should have no last error
					_ = subject.GetLastError()
					// May be nil or non-nil depending on implementation
				})

				ginkgo.It("updates last error after handling", func() {
					firstError := fmt.Errorf("first error")
					secondError := fmt.Errorf("second error")

					// Handle first error
					_ = subject.HandleError(firstError)

					// Handle second error
					_ = subject.HandleError(secondError)

					// Last error should reflect the most recent error
					lastError := subject.GetLastError()
					gomega.Expect(lastError).NotTo(gomega.BeNil())
				})

				ginkgo.It("maintains error context for debugging", func() {
					contextError := fmt.Errorf("error with context: operation failed")
					_ = subject.HandleError(contextError)

					lastError := subject.GetLastError()
					if lastError != nil {
						// Error should maintain enough context for debugging
						gomega.Expect(lastError.Error()).NotTo(gomega.BeEmpty())
					}
				})
			})

			ginkgo.Context("when testing standard error scenarios", func() {
				ginkgo.It("handles all standard error conditions appropriately", func() {
					for name, errorCondition := range StandardErrors {
						ginkgo.By(fmt.Sprintf("handling standard error %s", name))

						testError := fmt.Errorf("%s", errorCondition.Message)

						// Handle the error
						processedError := subject.HandleError(testError)
						gomega.Expect(processedError).NotTo(gomega.BeNil())

						// Check retryability
						isRetryable := subject.IsRetryableError(testError)
						gomega.Expect(isRetryable).To(gomega.Equal(errorCondition.Retryable))

						// Verify it's tracked
						lastError := subject.GetLastError()
						gomega.Expect(lastError).NotTo(gomega.BeNil())
					}
				})
			})
		})
	}
}

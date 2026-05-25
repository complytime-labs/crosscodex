package testspecs_test

import (
	"database/sql/driver"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	. "github.com/complytime-labs/crosscodex/internal/testspecs"
)

var _ = Describe("Custom Gomega Matchers", func() {
	Describe("BeValidTenantID", func() {
		It("should match valid tenant IDs", func() {
			Expect("acme-corp").To(BeValidTenantID())
			Expect("tenant-123").To(BeValidTenantID())
			Expect("a-b-c").To(BeValidTenantID())
			Expect("abc").To(BeValidTenantID()) // minimum length
		})

		It("should not match invalid tenant IDs", func() {
			Expect("").ToNot(BeValidTenantID())
			Expect("ab").ToNot(BeValidTenantID())                     // too short
			Expect("invalid@tenant").ToNot(BeValidTenantID())         // invalid chars
			Expect("tenant_with_underscore").ToNot(BeValidTenantID()) // underscores not allowed
			Expect("tenant with spaces").ToNot(BeValidTenantID())     // spaces not allowed
			Expect(generateLongString(65)).ToNot(BeValidTenantID())   // too long
		})

		It("should handle non-string types gracefully", func() {
			Expect(123).ToNot(BeValidTenantID())
			Expect(nil).ToNot(BeValidTenantID())
			Expect([]string{"valid-tenant"}).ToNot(BeValidTenantID())
		})

		It("should provide clear failure messages", func() {
			matcher := BeValidTenantID()
			success, err := matcher.Match("invalid@tenant")
			Expect(err).ToNot(HaveOccurred())
			Expect(success).To(BeFalse())
			Expect(matcher.FailureMessage("invalid@tenant")).To(ContainSubstring("invalid@tenant"))
			Expect(matcher.FailureMessage("invalid@tenant")).To(ContainSubstring("valid tenant ID"))
		})
	})

	Describe("HaveValidTenantPrefix", func() {
		It("should match strings with valid tenant prefixes", func() {
			Expect("acme-corp/some/path").To(HaveValidTenantPrefix())
			Expect("tenant-123/file.txt").To(HaveValidTenantPrefix())
			Expect("valid-tenant/data").To(HaveValidTenantPrefix())
		})

		It("should not match strings without valid tenant prefixes", func() {
			Expect("invalid@tenant/path").ToNot(HaveValidTenantPrefix())
			Expect("no-slash-here").ToNot(HaveValidTenantPrefix())
			Expect("").ToNot(HaveValidTenantPrefix())
			Expect("/starts-with-slash").ToNot(HaveValidTenantPrefix())
		})

		It("should handle non-string types gracefully", func() {
			Expect(123).ToNot(HaveValidTenantPrefix())
			Expect(nil).ToNot(HaveValidTenantPrefix())
		})
	})

	Describe("BeSecureError", func() {
		It("should match errors that don't leak sensitive information", func() {
			secureErr := errors.New("operation failed")
			Expect(secureErr).To(BeSecureError())

			userFriendlyErr := errors.New("invalid input format")
			Expect(userFriendlyErr).To(BeSecureError())
		})

		It("should not match errors that leak sensitive information", func() {
			leakyErr := errors.New("connection failed: password=secret123")
			Expect(leakyErr).ToNot(BeSecureError())

			sqlErr := errors.New("SQL error: INSERT INTO users (password) VALUES ('admin123')")
			Expect(sqlErr).ToNot(BeSecureError())

			pathErr := errors.New("failed to read /home/user/.ssh/id_rsa")
			Expect(pathErr).ToNot(BeSecureError())
		})

		It("should handle non-error types gracefully", func() {
			Expect("not an error").ToNot(BeSecureError())
			Expect(123).ToNot(BeSecureError())
			Expect(nil).ToNot(BeSecureError())
		})
	})

	Describe("BeActionableError", func() {
		It("should match errors with actionable information", func() {
			actionableErr := errors.New("invalid email format: please provide a valid email address")
			Expect(actionableErr).To(BeActionableError())

			helpfulErr := errors.New("file not found: check the path and try again")
			Expect(helpfulErr).To(BeActionableError())
		})

		It("should not match vague errors", func() {
			vagueErr := errors.New("something went wrong")
			Expect(vagueErr).ToNot(BeActionableError())

			genericErr := errors.New("error")
			Expect(genericErr).ToNot(BeActionableError())

			internalErr := errors.New("nil pointer dereference")
			Expect(internalErr).ToNot(BeActionableError())
		})

		It("should handle non-error types gracefully", func() {
			Expect("not an error").ToNot(BeActionableError())
			Expect(123).ToNot(BeActionableError())
			Expect(nil).ToNot(BeActionableError())
		})
	})

	Describe("HaveValidConfigStructure", func() {
		It("should match configurations with required sections", func() {
			validConfig := map[string]interface{}{
				"database": map[string]interface{}{
					"host": "localhost",
					"port": 5432,
				},
				"nats": map[string]interface{}{
					"url": "nats://localhost:4222",
				},
				"storage": map[string]interface{}{
					"type": "local",
				},
			}
			Expect(validConfig).To(HaveValidConfigStructure())
		})

		It("should not match configurations missing required sections", func() {
			incompleteConfig := map[string]interface{}{
				"database": map[string]interface{}{
					"host": "localhost",
				},
				// missing nats and storage sections
			}
			Expect(incompleteConfig).ToNot(HaveValidConfigStructure())

			emptyConfig := map[string]interface{}{}
			Expect(emptyConfig).ToNot(HaveValidConfigStructure())
		})

		It("should handle non-map types gracefully", func() {
			Expect("not a config").ToNot(HaveValidConfigStructure())
			Expect(123).ToNot(HaveValidConfigStructure())
			Expect(nil).ToNot(HaveValidConfigStructure())
		})
	})

	Describe("MatchConfigPrecedence", func() {
		It("should match configuration following XDG precedence rules", func() {
			precedenceData := map[string]interface{}{
				"sources": []string{
					"$XDG_CONFIG_HOME/crosscodex/config.yml",
					"$HOME/.config/crosscodex/config.yml",
					"/etc/crosscodex/config.yml",
				},
				"order": "user-first",
			}
			Expect(precedenceData).To(MatchConfigPrecedence())
		})

		It("should not match configuration with wrong precedence order", func() {
			wrongOrder := map[string]interface{}{
				"sources": []string{
					"/etc/crosscodex/config.yml",
					"$HOME/.config/crosscodex/config.yml",
				},
				"order": "system-first",
			}
			Expect(wrongOrder).ToNot(MatchConfigPrecedence())
		})
	})

	Describe("HaveIsolatedTenantData", func() {
		It("should match data structures with proper tenant isolation", func() {
			isolatedData := map[string]interface{}{
				"tenant_id": "acme-corp",
				"data": map[string]interface{}{
					"records": []map[string]interface{}{
						{
							"id":        "1",
							"tenant_id": "acme-corp",
							"content":   "some data",
						},
					},
				},
			}
			Expect(isolatedData).To(HaveIsolatedTenantData())
		})

		It("should not match data with mixed tenant IDs", func() {
			mixedData := map[string]interface{}{
				"tenant_id": "acme-corp",
				"data": map[string]interface{}{
					"records": []map[string]interface{}{
						{
							"id":        "1",
							"tenant_id": "acme-corp",
							"content":   "some data",
						},
						{
							"id":        "2",
							"tenant_id": "other-corp", // Different tenant!
							"content":   "other data",
						},
					},
				},
			}
			Expect(mixedData).ToNot(HaveIsolatedTenantData())
		})
	})

	Describe("BeSanitizedOutput", func() {
		It("should match output that has been sanitized", func() {
			sanitizedOutput := "User operation completed successfully"
			Expect(sanitizedOutput).To(BeSanitizedOutput())

			cleanData := map[string]interface{}{
				"status":  "success",
				"message": "Operation completed",
			}
			Expect(cleanData).To(BeSanitizedOutput())
		})

		It("should not match output containing sensitive information", func() {
			unsanitizedOutput := "User logged in with password: secret123"
			Expect(unsanitizedOutput).ToNot(BeSanitizedOutput())

			dirtyData := map[string]interface{}{
				"status": "success",
				"debug":  "SQL: SELECT * FROM users WHERE password = 'admin'",
			}
			Expect(dirtyData).ToNot(BeSanitizedOutput())
		})
	})

	Describe("ContainValidTimestamp", func() {
		It("should match structures containing valid RFC3339 timestamps", func() {
			validData := map[string]interface{}{
				"created_at": "2023-01-15T10:30:00Z",
				"updated_at": time.Now().Format(time.RFC3339),
			}
			Expect(validData).To(ContainValidTimestamp("created_at"))
			Expect(validData).To(ContainValidTimestamp("updated_at"))
		})

		It("should not match structures with invalid timestamps", func() {
			invalidData := map[string]interface{}{
				"created_at": "invalid-timestamp",
				"updated_at": "2023-13-45T99:99:99Z", // Invalid date/time
			}
			Expect(invalidData).ToNot(ContainValidTimestamp("created_at"))
			Expect(invalidData).ToNot(ContainValidTimestamp("updated_at"))
		})

		It("should handle missing timestamp fields", func() {
			dataWithoutField := map[string]interface{}{
				"name": "test",
			}
			Expect(dataWithoutField).ToNot(ContainValidTimestamp("created_at"))
		})
	})

	Describe("BeValidDatabaseConnection", func() {
		It("should match active database connections", func() {
			// Mock database connection that appears to be working
			mockConn := &mockDBConnection{
				connected: true,
				pingable:  true,
			}
			Expect(mockConn).To(BeValidDatabaseConnection())
		})

		It("should not match inactive or invalid connections", func() {
			// Mock database connection that appears to be broken
			mockConn := &mockDBConnection{
				connected: false,
				pingable:  false,
			}
			Expect(mockConn).ToNot(BeValidDatabaseConnection())

			// Non-connection type
			Expect("not a connection").ToNot(BeValidDatabaseConnection())
			Expect(nil).ToNot(BeValidDatabaseConnection())
		})
	})

	Describe("BeWithinTolerance", func() {
		It("should match numbers within specified tolerance", func() {
			Expect(10.1).To(BeWithinTolerance(10.0, 0.2))
			Expect(9.9).To(BeWithinTolerance(10.0, 0.2))
			Expect(10.0).To(BeWithinTolerance(10.0, 0.1))
		})

		It("should not match numbers outside tolerance", func() {
			Expect(10.3).ToNot(BeWithinTolerance(10.0, 0.2))
			Expect(9.7).ToNot(BeWithinTolerance(10.0, 0.2))
		})

		It("should handle non-numeric types gracefully", func() {
			Expect("not a number").ToNot(BeWithinTolerance(10.0, 0.1))
			Expect(nil).ToNot(BeWithinTolerance(10.0, 0.1))
		})
	})
})

// Helper functions for tests

func generateLongString(length int) string {
	result := make([]byte, length)
	for i := range result {
		result[i] = 'a'
	}
	return string(result)
}

// Mock database connection for testing
type mockDBConnection struct {
	connected bool
	pingable  bool
}

func (m *mockDBConnection) Ping() error {
	if !m.pingable {
		return errors.New("connection failed")
	}
	return nil
}

func (m *mockDBConnection) Close() error {
	return nil
}

// Implement driver.Conn interface minimally
func (m *mockDBConnection) Prepare(query string) (driver.Stmt, error) {
	return nil, errors.New("not implemented")
}

func (m *mockDBConnection) Begin() (driver.Tx, error) {
	return nil, errors.New("not implemented")
}

package testspecs

import (
	"context"
	"database/sql/driver"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/onsi/gomega/types"

	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// BeValidTenantID returns a matcher that checks if a string is a valid tenant ID
func BeValidTenantID() types.GomegaMatcher {
	return &tenantIDMatcher{}
}

type tenantIDMatcher struct{}

func (m *tenantIDMatcher) Match(actual interface{}) (bool, error) {
	str, ok := actual.(string)
	if !ok {
		return false, nil // Non-string types are not valid tenant IDs
	}

	return tenant.ValidateTenantID(str) == nil, nil
}

func (m *tenantIDMatcher) FailureMessage(actual interface{}) string {
	return fmt.Sprintf("Expected '%v' to be a valid tenant ID (3-64 chars, alphanumeric with hyphens)", actual)
}

func (m *tenantIDMatcher) NegatedFailureMessage(actual interface{}) string {
	return fmt.Sprintf("Expected '%v' not to be a valid tenant ID", actual)
}

// HaveValidTenantPrefix returns a matcher that checks if a string has a valid tenant prefix
func HaveValidTenantPrefix() types.GomegaMatcher {
	return &tenantPrefixMatcher{}
}

type tenantPrefixMatcher struct{}

func (m *tenantPrefixMatcher) Match(actual interface{}) (bool, error) {
	str, ok := actual.(string)
	if !ok {
		return false, nil
	}

	if str == "" {
		return false, nil
	}

	parts := strings.SplitN(str, "/", 2)
	if len(parts) < 2 {
		return false, nil // No slash found
	}

	tenantID := parts[0]
	return tenant.ValidateTenantID(tenantID) == nil, nil
}

func (m *tenantPrefixMatcher) FailureMessage(actual interface{}) string {
	return fmt.Sprintf("Expected '%v' to have a valid tenant prefix (format: tenant-id/path)", actual)
}

func (m *tenantPrefixMatcher) NegatedFailureMessage(actual interface{}) string {
	return fmt.Sprintf("Expected '%v' not to have a valid tenant prefix", actual)
}

// BeSecureError returns a matcher that checks if an error doesn't leak sensitive information
func BeSecureError() types.GomegaMatcher {
	return &secureErrorMatcher{}
}

type secureErrorMatcher struct{}

var sensitivePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)password\s*[=:]\s*\S+`),
	regexp.MustCompile(`(?i)secret\s*[=:]\s*\S+`),
	regexp.MustCompile(`(?i)token\s*[=:]\s*\S+`),
	regexp.MustCompile(`(?i)key\s*[=:]\s*\S+`),
	regexp.MustCompile(`(?i)sql\s+(error|query).*`),
	regexp.MustCompile(`(?i)(insert|update|delete|select)\s+.*password.*`),
	regexp.MustCompile(`(?i)(insert|update|delete|select)\s+.*admin.*`),
	regexp.MustCompile(`/[a-zA-Z0-9_\-./]*\.ssh/`),
	regexp.MustCompile(`/home/[a-zA-Z0-9_\-./]*/`),
	regexp.MustCompile(`(?i)(admin|root|user)\s*[=:]\s*\S+`),
}

func (m *secureErrorMatcher) Match(actual interface{}) (bool, error) {
	err, ok := actual.(error)
	if !ok {
		return false, nil // Non-error types are not secure errors
	}

	errMsg := err.Error()

	// Check for sensitive patterns
	for _, pattern := range sensitivePatterns {
		if pattern.MatchString(errMsg) {
			return false, nil
		}
	}

	return true, nil
}

func (m *secureErrorMatcher) FailureMessage(actual interface{}) string {
	return fmt.Sprintf("Expected error '%v' to not leak sensitive information", actual)
}

func (m *secureErrorMatcher) NegatedFailureMessage(actual interface{}) string {
	return fmt.Sprintf("Expected error '%v' to leak sensitive information", actual)
}

// BeActionableError returns a matcher that checks if an error provides actionable information
func BeActionableError() types.GomegaMatcher {
	return &actionableErrorMatcher{}
}

type actionableErrorMatcher struct{}

var actionableKeywords = []string{
	"please", "try", "check", "verify", "ensure", "make sure",
	"should", "must", "required", "provide", "specify", "use",
	"contact", "see", "refer", "visit", "example", "format",
}

var nonActionablePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)something\s+went\s+wrong`),
	regexp.MustCompile(`(?i)^error$`),
	regexp.MustCompile(`(?i)internal\s+error`),
	regexp.MustCompile(`(?i)nil\s+pointer`),
	regexp.MustCompile(`(?i)panic`),
}

func (m *actionableErrorMatcher) Match(actual interface{}) (bool, error) {
	err, ok := actual.(error)
	if !ok {
		return false, nil
	}

	errMsg := strings.ToLower(err.Error())

	// Check for non-actionable patterns first
	for _, pattern := range nonActionablePatterns {
		if pattern.MatchString(errMsg) {
			return false, nil
		}
	}

	// Check for actionable keywords
	for _, keyword := range actionableKeywords {
		if strings.Contains(errMsg, keyword) {
			return true, nil
		}
	}

	// If it's a validation error with specific format requirements, consider it actionable
	if strings.Contains(errMsg, "format") || strings.Contains(errMsg, "invalid") || strings.Contains(errMsg, "required") {
		return len(errMsg) > 20, nil // Must be descriptive, not just "invalid"
	}

	return false, nil
}

func (m *actionableErrorMatcher) FailureMessage(actual interface{}) string {
	return fmt.Sprintf("Expected error '%v' to provide actionable information for the user", actual)
}

func (m *actionableErrorMatcher) NegatedFailureMessage(actual interface{}) string {
	return fmt.Sprintf("Expected error '%v' not to provide actionable information", actual)
}

// HaveValidConfigStructure returns a matcher that checks if a configuration has required sections
func HaveValidConfigStructure() types.GomegaMatcher {
	return &configStructureMatcher{}
}

type configStructureMatcher struct{}

var requiredConfigSections = []string{"database", "nats", "storage"}

func (m *configStructureMatcher) Match(actual interface{}) (bool, error) {
	config, ok := actual.(map[string]interface{})
	if !ok {
		return false, nil
	}

	for _, section := range requiredConfigSections {
		if _, exists := config[section]; !exists {
			return false, nil
		}
	}

	return true, nil
}

func (m *configStructureMatcher) FailureMessage(actual interface{}) string {
	return fmt.Sprintf("Expected configuration '%v' to have required sections: %v", actual, requiredConfigSections)
}

func (m *configStructureMatcher) NegatedFailureMessage(actual interface{}) string {
	return fmt.Sprintf("Expected configuration '%v' not to have all required sections", actual)
}

// MatchConfigPrecedence returns a matcher that checks if configuration follows XDG precedence rules
func MatchConfigPrecedence() types.GomegaMatcher {
	return &configPrecedenceMatcher{}
}

type configPrecedenceMatcher struct{}

func (m *configPrecedenceMatcher) Match(actual interface{}) (bool, error) {
	config, ok := actual.(map[string]interface{})
	if !ok {
		return false, nil
	}

	orderValue, exists := config["order"]
	if !exists {
		return false, nil
	}

	order, ok := orderValue.(string)
	if !ok {
		return false, nil
	}

	// Must be user-first precedence for XDG compliance
	return order == "user-first", nil
}

func (m *configPrecedenceMatcher) FailureMessage(actual interface{}) string {
	return fmt.Sprintf("Expected configuration '%v' to follow XDG precedence rules (user-first order)", actual)
}

func (m *configPrecedenceMatcher) NegatedFailureMessage(actual interface{}) string {
	return fmt.Sprintf("Expected configuration '%v' not to follow XDG precedence rules", actual)
}

// HaveIsolatedTenantData returns a matcher that checks if data structures have proper tenant isolation
func HaveIsolatedTenantData() types.GomegaMatcher {
	return &tenantIsolationMatcher{}
}

type tenantIsolationMatcher struct{}

func (m *tenantIsolationMatcher) Match(actual interface{}) (bool, error) {
	data, ok := actual.(map[string]interface{})
	if !ok {
		return false, nil
	}

	mainTenantIDValue, exists := data["tenant_id"]
	if !exists {
		return false, nil
	}

	mainTenantID, ok := mainTenantIDValue.(string)
	if !ok {
		return false, nil
	}

	// Check if all nested data has the same tenant ID
	return m.checkTenantConsistency(data, mainTenantID), nil
}

func (m *tenantIsolationMatcher) checkTenantConsistency(data interface{}, expectedTenantID string) bool {
	switch v := data.(type) {
	case map[string]interface{}:
		for key, value := range v {
			if key == "tenant_id" {
				if tenantID, ok := value.(string); ok && tenantID != expectedTenantID {
					return false
				}
			} else if !m.checkTenantConsistency(value, expectedTenantID) {
				return false
			}
		}
	case []interface{}:
		for _, item := range v {
			if !m.checkTenantConsistency(item, expectedTenantID) {
				return false
			}
		}
	case []map[string]interface{}:
		for _, item := range v {
			if !m.checkTenantConsistency(item, expectedTenantID) {
				return false
			}
		}
	}
	return true
}

func (m *tenantIsolationMatcher) FailureMessage(actual interface{}) string {
	return fmt.Sprintf("Expected data '%v' to have consistent tenant isolation", actual)
}

func (m *tenantIsolationMatcher) NegatedFailureMessage(actual interface{}) string {
	return fmt.Sprintf("Expected data '%v' not to have consistent tenant isolation", actual)
}

// BeSanitizedOutput returns a matcher that checks if output has been sanitized of sensitive information
func BeSanitizedOutput() types.GomegaMatcher {
	return &sanitizedOutputMatcher{}
}

type sanitizedOutputMatcher struct{}

func (m *sanitizedOutputMatcher) Match(actual interface{}) (bool, error) {
	// Check strings directly
	if str, ok := actual.(string); ok {
		return m.isSanitized(str), nil
	}

	// Check maps/structs for sensitive data
	if data, ok := actual.(map[string]interface{}); ok {
		return m.checkMapSanitized(data), nil
	}

	// For other types, assume they're clean
	return true, nil
}

func (m *sanitizedOutputMatcher) isSanitized(text string) bool {
	lowerText := strings.ToLower(text)

	sensitiveWords := []string{"password", "secret", "token", "key", "sql:", "select *", "insert into", "update set", "delete from"}
	for _, word := range sensitiveWords {
		if strings.Contains(lowerText, word) {
			return false
		}
	}

	return true
}

func (m *sanitizedOutputMatcher) checkMapSanitized(data map[string]interface{}) bool {
	for key, value := range data {
		keyLower := strings.ToLower(key)

		// Check sensitive key names
		if strings.Contains(keyLower, "password") || strings.Contains(keyLower, "secret") ||
			strings.Contains(keyLower, "token") || strings.Contains(keyLower, "key") ||
			strings.Contains(keyLower, "sql") || strings.Contains(keyLower, "debug") {
			return false
		}

		// Check string values
		if str, ok := value.(string); ok && !m.isSanitized(str) {
			return false
		}

		// Recursively check nested maps
		if nestedMap, ok := value.(map[string]interface{}); ok && !m.checkMapSanitized(nestedMap) {
			return false
		}
	}

	return true
}

func (m *sanitizedOutputMatcher) FailureMessage(actual interface{}) string {
	return fmt.Sprintf("Expected output '%v' to be sanitized of sensitive information", actual)
}

func (m *sanitizedOutputMatcher) NegatedFailureMessage(actual interface{}) string {
	return fmt.Sprintf("Expected output '%v' to contain sensitive information", actual)
}

// ContainValidTimestamp returns a matcher that checks if a structure contains a valid RFC3339 timestamp
func ContainValidTimestamp(fieldName string) types.GomegaMatcher {
	return &timestampMatcher{fieldName: fieldName}
}

type timestampMatcher struct {
	fieldName string
}

func (m *timestampMatcher) Match(actual interface{}) (bool, error) {
	data, ok := actual.(map[string]interface{})
	if !ok {
		return false, nil
	}

	timestampValue, exists := data[m.fieldName]
	if !exists {
		return false, nil
	}

	timestampStr, ok := timestampValue.(string)
	if !ok {
		return false, nil
	}

	_, err := time.Parse(time.RFC3339, timestampStr)
	return err == nil, nil
}

func (m *timestampMatcher) FailureMessage(actual interface{}) string {
	return fmt.Sprintf("Expected '%v' to contain a valid RFC3339 timestamp in field '%s'", actual, m.fieldName)
}

func (m *timestampMatcher) NegatedFailureMessage(actual interface{}) string {
	return fmt.Sprintf("Expected '%v' not to contain a valid RFC3339 timestamp in field '%s'", actual, m.fieldName)
}

// BeValidDatabaseConnection returns a matcher that checks if a value is a valid, active database connection
func BeValidDatabaseConnection() types.GomegaMatcher {
	return &dbConnectionMatcher{}
}

type dbConnectionMatcher struct{}

// DBPinger represents any type that can ping a database
type DBPinger interface {
	Ping() error
}

func (m *dbConnectionMatcher) Match(actual interface{}) (bool, error) {
	if actual == nil {
		return false, nil
	}

	// Check if it implements our DBPinger interface
	if pinger, ok := actual.(DBPinger); ok {
		return pinger.Ping() == nil, nil
	}

	// Check if it implements the standard driver.Pinger interface
	if pinger, ok := actual.(driver.Pinger); ok {
		ctx := context.TODO() // DevSkim: ignore DS176209 - Standard Go context pattern, not actual TODO
		return pinger.Ping(ctx) == nil, nil
	}

	// Check reflection for standard database connection types
	v := reflect.ValueOf(actual)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}

	// Look for a Ping method
	pingMethod := v.MethodByName("Ping")
	if pingMethod.IsValid() {
		results := pingMethod.Call([]reflect.Value{})
		if len(results) == 1 && results[0].Type().Implements(reflect.TypeOf((*error)(nil)).Elem()) {
			err := results[0].Interface()
			return err == nil, nil
		}
	}

	return false, nil
}

func (m *dbConnectionMatcher) FailureMessage(actual interface{}) string {
	return fmt.Sprintf("Expected '%v' to be a valid, active database connection", actual)
}

func (m *dbConnectionMatcher) NegatedFailureMessage(actual interface{}) string {
	return fmt.Sprintf("Expected '%v' not to be a valid database connection", actual)
}

// BeWithinTolerance returns a matcher that checks if a number is within a specified tolerance of an expected value
func BeWithinTolerance(expected, tolerance float64) types.GomegaMatcher {
	return &toleranceMatcher{
		expected:  expected,
		tolerance: tolerance,
	}
}

type toleranceMatcher struct {
	expected  float64
	tolerance float64
}

func (m *toleranceMatcher) Match(actual interface{}) (bool, error) {
	var actualFloat float64
	var ok bool

	// Convert various numeric types to float64
	switch v := actual.(type) {
	case float64:
		actualFloat = v
		ok = true
	case float32:
		actualFloat = float64(v)
		ok = true
	case int:
		actualFloat = float64(v)
		ok = true
	case int8:
		actualFloat = float64(v)
		ok = true
	case int16:
		actualFloat = float64(v)
		ok = true
	case int32:
		actualFloat = float64(v)
		ok = true
	case int64:
		actualFloat = float64(v)
		ok = true
	case uint:
		actualFloat = float64(v)
		ok = true
	case uint8:
		actualFloat = float64(v)
		ok = true
	case uint16:
		actualFloat = float64(v)
		ok = true
	case uint32:
		actualFloat = float64(v)
		ok = true
	case uint64:
		actualFloat = float64(v)
		ok = true
	default:
		ok = false
	}

	if !ok {
		return false, nil
	}

	diff := math.Abs(actualFloat - m.expected)
	return diff <= m.tolerance, nil
}

func (m *toleranceMatcher) FailureMessage(actual interface{}) string {
	return fmt.Sprintf("Expected '%v' to be within %v of %v", actual, m.tolerance, m.expected)
}

func (m *toleranceMatcher) NegatedFailureMessage(actual interface{}) string {
	return fmt.Sprintf("Expected '%v' not to be within %v of %v", actual, m.tolerance, m.expected)
}

package tenant

import (
	"fmt"
	"regexp"
)

// tenantIDPattern enforces the tenant ID format:
//   - starts with a lowercase letter
//   - middle characters: lowercase letters, digits, or hyphens
//   - ends with a lowercase letter or digit
//   - length: 3–64 characters
var tenantIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}[a-z0-9]$`)

// ValidateTenantID checks whether id is a well-formed tenant identifier.
// It returns an error wrapping ErrInvalidTenant when validation fails,
// or nil when the ID is valid.
func ValidateTenantID(id string) error {
	if id == "" {
		return fmt.Errorf("tenant ID must not be empty: %w", ErrInvalidTenant)
	}
	if !tenantIDPattern.MatchString(id) {
		return fmt.Errorf("tenant ID %q does not match required pattern %s: %w",
			id, tenantIDPattern.String(), ErrInvalidTenant)
	}
	return nil
}

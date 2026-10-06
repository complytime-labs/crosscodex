package tenant

import (
	"fmt"
	"regexp"
)

// tenantIDPattern enforces the tenant ID format:
//   - starts with a lowercase letter
//   - middle characters: lowercase letters, digits, or hyphens
//   - ends with a lowercase letter or digit
//   - length: 3–52 characters
//
// 52 is the longest ID whose Apache AGE graph name, "crosscodex_" + ID, fits
// PostgreSQL's 63-byte identifier limit (NAMEDATALEN-1). PostgreSQL silently
// truncates longer names, so two longer IDs sharing a 52-character prefix
// would map to the same graph. Tenants provisioned with a longer ID before
// #148 must be re-provisioned; see "Upgrade note (#148)" in
// docs/dev/design-principles.md.
var tenantIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,50}[a-z0-9]$`)

// IDRule states the tenant ID format in words, for error messages.
const IDRule = "3-52 characters, lowercase letters, digits and hyphens, starting with a letter and not ending with a hyphen"

// ValidateTenantID checks whether id is a well-formed tenant identifier.
// It returns an error wrapping ErrInvalidTenant when validation fails,
// or nil when the ID is valid.
func ValidateTenantID(id string) error {
	if id == "" {
		return fmt.Errorf("tenant ID must not be empty: %w", ErrInvalidTenant)
	}
	if !tenantIDPattern.MatchString(id) {
		return &invalidIDError{id: id}
	}
	return nil
}

// invalidIDError is a malformed tenant ID. It wraps ErrInvalidTenant
// without appending that sentinel's text, so a caller that wraps it again
// (graphdb.CheckTenant) can reuse the message as is.
type invalidIDError struct{ id string }

func (e *invalidIDError) Error() string {
	return fmt.Sprintf("tenant ID %q is invalid: it must be %s. Choose an ID that matches", e.id, IDRule)
}

func (e *invalidIDError) Unwrap() error { return ErrInvalidTenant }

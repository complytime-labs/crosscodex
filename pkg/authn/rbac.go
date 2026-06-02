package authn

import "strings"

// RequireRole checks whether the identity holds at least one of the required roles.
// Returns ErrInsufficientRole if no match is found.
//
// Both identity roles and required roles are trimmed of whitespace before comparison.
// Whitespace-only roles match nothing. Calling with zero required roles returns an error
// (caller bug — fail closed).
func RequireRole(id Identity, roles ...string) error {
	if len(roles) == 0 {
		return ErrInsufficientRole
	}

	for _, required := range roles {
		required = strings.TrimSpace(required)
		if required == "" {
			continue
		}
		for _, have := range id.Roles {
			if strings.TrimSpace(have) == required {
				return nil
			}
		}
	}

	return ErrInsufficientRole
}

// IsAdmin reports whether the identity holds the admin role.
func IsAdmin(id Identity) bool {
	return RequireRole(id, RoleAdmin) == nil
}

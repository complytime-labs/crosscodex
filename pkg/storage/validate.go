package storage

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

func validateKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: empty key", ErrInvalidKey)
	}
	if filepath.IsAbs(key) {
		return fmt.Errorf("%w: absolute path not allowed", ErrInvalidKey)
	}
	if strings.ContainsRune(key, 0) {
		return fmt.Errorf("%w: null byte in key", ErrInvalidKey)
	}
	if strings.ContainsRune(key, '\\') {
		return fmt.Errorf("%w: backslash not allowed", ErrInvalidKey)
	}
	for _, segment := range strings.Split(key, "/") {
		if segment == ".." {
			return fmt.Errorf("%w: path traversal not allowed", ErrInvalidKey)
		}
	}
	cleaned := filepath.Clean(key)
	if cleaned == "." {
		return fmt.Errorf("%w: key resolves to current directory", ErrInvalidKey)
	}
	if strings.HasPrefix(cleaned, "..") {
		return fmt.Errorf("%w: path traversal not allowed", ErrInvalidKey)
	}
	return nil
}

func validateTenantID(tenantID string) error {
	return tenant.ValidateTenantID(tenantID)
}

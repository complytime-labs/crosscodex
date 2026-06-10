package llmclient

import (
	"fmt"
	"os"
	"strings"
)

// ResolveCredential resolves a credential reference URI to a plaintext value.
//
// Supported schemes:
//   - env:VAR_NAME — reads from environment variable VAR_NAME
//   - file:/path/to/file — reads from file; file must have mode 0600 or stricter
//   - vault:path/key — reserved for Vault integration (not yet implemented)
//
// Returns ErrCredentialResolution on failure with an actionable message.
func ResolveCredential(ref string) (string, error) {
	if ref == "" {
		return "", fmt.Errorf("empty credential reference: %w", ErrCredentialResolution)
	}

	scheme, value, ok := strings.Cut(ref, ":")
	if !ok {
		return "", fmt.Errorf("credential reference %q missing scheme (expected env:, file:, or vault:): %w",
			ref, ErrCredentialResolution)
	}

	switch scheme {
	case "env":
		return resolveEnv(value)
	case "file":
		return resolveFile(value)
	case "vault":
		return resolveVault(value)
	default:
		return "", fmt.Errorf("unsupported credential scheme %q (supported: env, file, vault): %w",
			scheme, ErrCredentialResolution)
	}
}

func resolveEnv(varName string) (string, error) {
	if varName == "" {
		return "", fmt.Errorf("env: credential reference has empty variable name: %w", ErrCredentialResolution)
	}
	val, ok := os.LookupEnv(varName)
	if !ok {
		return "", fmt.Errorf("environment variable %q is not set: %w", varName, ErrCredentialResolution)
	}
	if val == "" {
		return "", fmt.Errorf("environment variable %q is set but empty: %w", varName, ErrCredentialResolution)
	}
	return val, nil
}

func resolveFile(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("file: credential reference has empty path: %w", ErrCredentialResolution)
	}

	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("credential file %q: %w: %w", path, err, ErrCredentialResolution)
	}

	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("credential path %q is not a regular file: %w", path, ErrCredentialResolution)
	}

	mode := info.Mode().Perm()
	if mode&0077 != 0 {
		return "", fmt.Errorf("credential file %q has permissions %04o, must be 0600 or stricter (no group/other access): %w",
			path, mode, ErrCredentialResolution)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading credential file %q: %w: %w", path, err, ErrCredentialResolution)
	}

	return strings.TrimSpace(string(data)), nil
}

func resolveVault(path string) (string, error) {
	return "", fmt.Errorf("vault credential resolution is not implemented; "+
		"configure env: or file: credential reference instead of vault:%s: %w",
		path, ErrCredentialResolution)
}

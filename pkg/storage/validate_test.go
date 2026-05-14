package storage

import (
	"errors"
	"testing"
)

func TestValidateKey(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		wantErr error
	}{
		{name: "empty key", key: "", wantErr: ErrInvalidKey},
		{name: "absolute path", key: "/etc/passwd", wantErr: ErrInvalidKey},
		{name: "simple traversal", key: "../other-tenant/secret", wantErr: ErrInvalidKey},
		{name: "mid-path traversal", key: "docs/../../other/secret", wantErr: ErrInvalidKey},
		{name: "trailing traversal", key: "docs/..", wantErr: ErrInvalidKey},
		{name: "dot only", key: ".", wantErr: ErrInvalidKey},
		{name: "double dot only", key: "..", wantErr: ErrInvalidKey},
		{name: "null byte", key: "docs/\x00evil", wantErr: ErrInvalidKey},
		{name: "backslash traversal", key: "docs\\..\\secret", wantErr: ErrInvalidKey},
		{name: "valid flat", key: "file.json", wantErr: nil},
		{name: "valid nested", key: "documents/sub/file.json", wantErr: nil},
		{name: "valid dotfile", key: "documents/.hidden", wantErr: nil},
		{name: "valid deep path", key: "a/b/c/d/e.json", wantErr: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateKey(tt.key)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("validateKey(%q) error = %v, want %v", tt.key, err, tt.wantErr)
				}
			} else if err != nil {
				t.Errorf("validateKey(%q) unexpected error: %v", tt.key, err)
			}
		})
	}
}

func TestValidateTenantID(t *testing.T) {
	tests := []struct {
		name     string
		tenantID string
		wantErr  error
	}{
		{name: "empty tenant", tenantID: "", wantErr: ErrTenantRequired},
		{name: "valid tenant", tenantID: "acme-corp", wantErr: nil},
		{name: "uuid tenant", tenantID: "550e8400-e29b-41d4-a716-446655440000", wantErr: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTenantID(tt.tenantID)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("validateTenantID(%q) error = %v, want %v", tt.tenantID, err, tt.wantErr)
				}
			} else if err != nil {
				t.Errorf("validateTenantID(%q) unexpected error: %v", tt.tenantID, err)
			}
		})
	}
}

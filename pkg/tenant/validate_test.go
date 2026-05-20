package tenant

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateTenantID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		id      string
		wantErr bool
	}{
		// Valid cases
		{name: "simple lowercase", id: "abc", wantErr: false},
		{name: "with hyphen and digit", id: "my-tenant-1", wantErr: false},
		{name: "mixed letters and digits", id: "a1b2c3", wantErr: false},
		{name: "minimum length 3 chars", id: "a1b", wantErr: false},
		{name: "exactly 63 chars", id: "a" + strings.Repeat("b", 61) + "c", wantErr: false},
		{name: "maximum length 64 chars", id: "a" + strings.Repeat("b", 62) + "c", wantErr: false},
		{name: "consecutive hyphens allowed", id: "a--b", wantErr: false},
		{name: "all hyphens middle", id: "a---b", wantErr: false},

		// Invalid: empty
		{name: "empty string", id: "", wantErr: true},

		// Invalid: length
		{name: "too short 2 chars", id: "ab", wantErr: true},
		{name: "too short single char", id: "a", wantErr: true},
		{name: "too long 65 chars", id: "a" + strings.Repeat("b", 63) + "c", wantErr: true},

		// Invalid: character constraints
		{name: "uppercase letters", id: "MyTenant", wantErr: true},
		{name: "underscore", id: "my_tenant", wantErr: true},
		{name: "dot", id: "my.tenant", wantErr: true},
		{name: "at sign", id: "my@tenant", wantErr: true},

		// Invalid: boundary characters
		{name: "leading hyphen", id: "-abc", wantErr: true},
		{name: "trailing hyphen", id: "abc-", wantErr: true},
		{name: "leading digit", id: "1abc", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateTenantID(tt.id)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("ValidateTenantID(%q) = nil, want error", tt.id)
				}
				if !errors.Is(err, ErrInvalidTenant) {
					t.Errorf("ValidateTenantID(%q) error = %v, want errors.Is ErrInvalidTenant", tt.id, err)
				}
			} else {
				if err != nil {
					t.Errorf("ValidateTenantID(%q) = %v, want nil", tt.id, err)
				}
			}
		})
	}
}

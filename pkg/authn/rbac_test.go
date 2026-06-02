package authn_test

import (
	"errors"
	"testing"

	"github.com/complytime-labs/crosscodex/pkg/authn"
)

func TestRequireRole(t *testing.T) {
	tests := []struct {
		name     string
		roles    []string
		required []string
		wantErr  bool
	}{
		{
			name:     "exact single match",
			roles:    []string{"admin"},
			required: []string{"admin"},
			wantErr:  false,
		},
		{
			name:     "any-match from multiple required",
			roles:    []string{"reader"},
			required: []string{"admin", "reader"},
			wantErr:  false,
		},
		{
			name:     "any-match from multiple identity roles",
			roles:    []string{"writer", "admin"},
			required: []string{"admin"},
			wantErr:  false,
		},
		{
			name:     "no match",
			roles:    []string{"reader"},
			required: []string{"admin"},
			wantErr:  true,
		},
		{
			name:     "empty identity roles",
			roles:    []string{},
			required: []string{"admin"},
			wantErr:  true,
		},
		{
			name:     "nil identity roles",
			roles:    nil,
			required: []string{"admin"},
			wantErr:  true,
		},
		{
			name:     "zero required roles is caller bug",
			roles:    []string{"admin"},
			required: []string{},
			wantErr:  true,
		},
		{
			name:     "case sensitive Admin vs admin",
			roles:    []string{"Admin"},
			required: []string{"admin"},
			wantErr:  true,
		},
		{
			name:     "whitespace stripped from identity role",
			roles:    []string{" admin "},
			required: []string{"admin"},
			wantErr:  false,
		},
		{
			name:     "whitespace stripped from required role",
			roles:    []string{"admin"},
			required: []string{" admin "},
			wantErr:  false,
		},
		{
			name:     "whitespace-only identity role matches nothing",
			roles:    []string{"  "},
			required: []string{"admin"},
			wantErr:  true,
		},
		{
			name:     "whitespace-only required role matches nothing",
			roles:    []string{"admin"},
			required: []string{"  "},
			wantErr:  true,
		},
		{
			name:     "duplicates are harmless",
			roles:    []string{"admin", "admin"},
			required: []string{"admin", "admin"},
			wantErr:  false,
		},
		{
			name:     "long role string",
			roles:    []string{"super-long-role-name-that-is-still-valid"},
			required: []string{"super-long-role-name-that-is-still-valid"},
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := authn.Identity{Roles: tt.roles}
			err := authn.RequireRole(id, tt.required...)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if !errors.Is(err, authn.ErrInsufficientRole) {
					t.Errorf("got error %v, want ErrInsufficientRole", err)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestIsAdmin(t *testing.T) {
	tests := []struct {
		name  string
		roles []string
		want  bool
	}{
		{
			name:  "admin present",
			roles: []string{"admin"},
			want:  true,
		},
		{
			name:  "admin absent",
			roles: []string{"reader", "writer"},
			want:  false,
		},
		{
			name:  "empty roles",
			roles: []string{},
			want:  false,
		},
		{
			name:  "nil roles",
			roles: nil,
			want:  false,
		},
		{
			name:  "case sensitive Admin",
			roles: []string{"Admin"},
			want:  false,
		},
		{
			name:  "whitespace stripped",
			roles: []string{" admin "},
			want:  true,
		},
		{
			name:  "admin among many",
			roles: []string{"reader", "writer", "admin", "operator"},
			want:  true,
		},
		{
			name:  "administrator is not admin",
			roles: []string{"administrator"},
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := authn.Identity{Roles: tt.roles}
			got := authn.IsAdmin(id)
			if got != tt.want {
				t.Errorf("IsAdmin() = %v, want %v", got, tt.want)
			}
		})
	}
}

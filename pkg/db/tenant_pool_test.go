package db

import (
	"context"
	"errors"
	"testing"
)

func TestTenantFromContext_Empty(t *testing.T) {
	ctx := context.Background()
	if got := TenantFromContext(ctx); got != "" {
		t.Errorf("TenantFromContext(empty) = %q, want empty", got)
	}
}

func TestTenantFromContext_Set(t *testing.T) {
	ctx := ContextWithTenant(context.Background(), "acme")
	if got := TenantFromContext(ctx); got != "acme" {
		t.Errorf("TenantFromContext = %q, want acme", got)
	}
}

func TestUserFromContext_Empty(t *testing.T) {
	ctx := context.Background()
	if got := UserFromContext(ctx); got != "" {
		t.Errorf("UserFromContext(empty) = %q, want empty", got)
	}
}

func TestUserFromContext_Set(t *testing.T) {
	ctx := ContextWithUser(context.Background(), "alice")
	if got := UserFromContext(ctx); got != "alice" {
		t.Errorf("UserFromContext = %q, want alice", got)
	}
}

func TestTenantPool_QueryReturnsError(t *testing.T) {
	tp := &tenantPool{}
	_, err := tp.Query(context.Background(), "SELECT 1")
	if !errors.Is(err, ErrTenantRequired) {
		t.Errorf("Query error = %v, want ErrTenantRequired", err)
	}
}

func TestTenantPool_ExecReturnsError(t *testing.T) {
	tp := &tenantPool{}
	err := tp.Exec(context.Background(), "SELECT 1")
	if !errors.Is(err, ErrTenantRequired) {
		t.Errorf("Exec error = %v, want ErrTenantRequired", err)
	}
}

func TestTenantPool_QueryRowReturnsError(t *testing.T) {
	tp := &tenantPool{}
	row := tp.QueryRow(context.Background(), "SELECT 1")
	err := row.Scan()
	if !errors.Is(err, ErrTenantRequired) {
		t.Errorf("QueryRow.Scan error = %v, want ErrTenantRequired", err)
	}
}

func TestTenantPool_BeginNoTenant(t *testing.T) {
	tp := &tenantPool{}
	_, err := tp.Begin(context.Background())
	if !errors.Is(err, ErrTenantRequired) {
		t.Errorf("Begin error = %v, want ErrTenantRequired", err)
	}
}

func TestErrRow_Scan(t *testing.T) {
	r := &errRow{err: ErrTenantRequired}
	if err := r.Scan("a", "b"); !errors.Is(err, ErrTenantRequired) {
		t.Errorf("errRow.Scan = %v, want ErrTenantRequired", err)
	}
}

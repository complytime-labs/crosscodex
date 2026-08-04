package db

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeConn is a test double for Connection that records the last Exec call
// and returns a configurable error. Only Exec is exercised by EnsureTenant;
// the other methods panic to fail loudly if the contract changes.
type fakeConn struct {
	execCalls int
	lastQuery string
	lastArgs  []any
	execErr   error
}

func (f *fakeConn) Exec(_ context.Context, query string, args ...any) error {
	f.execCalls++
	f.lastQuery = query
	f.lastArgs = args
	return f.execErr
}
func (f *fakeConn) Begin(context.Context) (Transaction, error)      { panic("fakeConn.Begin unused") }
func (f *fakeConn) Query(context.Context, string, ...any) (Rows, error) { panic("fakeConn.Query unused") }
func (f *fakeConn) QueryRow(context.Context, string, ...any) Row    { panic("fakeConn.QueryRow unused") }
func (f *fakeConn) Close() error                                    { return nil }

func TestEnsureTenant_IssuesInsertWithArgs(t *testing.T) {
	f := &fakeConn{}
	if err := EnsureTenant(context.Background(), f, "acme", "Acme Corp"); err != nil {
		t.Fatalf("EnsureTenant returned error: %v", err)
	}
	if f.execCalls != 1 {
		t.Fatalf("expected exactly 1 Exec call, got %d", f.execCalls)
	}
	if !strings.Contains(f.lastQuery, "INSERT INTO tenants") ||
		!strings.Contains(f.lastQuery, "ON CONFLICT") {
		t.Fatalf("unexpected query: %q", f.lastQuery)
	}
	if len(f.lastArgs) != 2 || f.lastArgs[0] != "acme" || f.lastArgs[1] != "Acme Corp" {
		t.Fatalf("unexpected args: %#v", f.lastArgs)
	}
}

func TestEnsureTenant_RejectsEmptyTenantID(t *testing.T) {
	f := &fakeConn{}
	err := EnsureTenant(context.Background(), f, "", "Whatever")
	if err == nil {
		t.Fatal("expected error for empty tenant ID, got nil")
	}
	if f.execCalls != 0 {
		t.Fatalf("expected no Exec call for empty tenant ID, got %d", f.execCalls)
	}
}

func TestEnsureTenant_WrapsExecError(t *testing.T) {
	sentinel := errors.New("boom")
	f := &fakeConn{execErr: sentinel}
	err := EnsureTenant(context.Background(), f, "acme", "Acme Corp")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped sentinel, got %v", err)
	}
	if !strings.Contains(err.Error(), `ensure tenant "acme"`) {
		t.Fatalf("expected error to name the tenant, got %q", err.Error())
	}
}

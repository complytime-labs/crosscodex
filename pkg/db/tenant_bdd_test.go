package db

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
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
func (f *fakeConn) Begin(context.Context) (Transaction, error) { panic("fakeConn.Begin unused") }
func (f *fakeConn) Query(context.Context, string, ...any) (Rows, error) {
	panic("fakeConn.Query unused")
}
func (f *fakeConn) QueryRow(context.Context, string, ...any) Row { panic("fakeConn.QueryRow unused") }
func (f *fakeConn) Close() error                                 { return nil }

var _ = Describe("EnsureTenant", func() {
	It("issues an insert with the tenant args", func() {
		f := &fakeConn{}
		Expect(EnsureTenant(context.Background(), f, "acme", "Acme Corp")).To(Succeed())
		Expect(f.execCalls).To(Equal(1))
		Expect(f.lastQuery).To(ContainSubstring("INSERT INTO tenants"))
		Expect(f.lastQuery).To(ContainSubstring("ON CONFLICT"))
		Expect(f.lastArgs).To(Equal([]any{"acme", "Acme Corp"}))
	})

	It("rejects an empty tenant ID", func() {
		f := &fakeConn{}
		err := EnsureTenant(context.Background(), f, "", "Whatever")
		Expect(err).To(HaveOccurred())
		Expect(f.execCalls).To(Equal(0))
	})

	It("wraps the exec error", func() {
		sentinel := errors.New("boom")
		f := &fakeConn{execErr: sentinel}
		err := EnsureTenant(context.Background(), f, "acme", "Acme Corp")
		Expect(err).To(HaveOccurred())
		Expect(errors.Is(err, sentinel)).To(BeTrue())
		Expect(err.Error()).To(ContainSubstring(`ensure tenant "acme"`))
	})
})

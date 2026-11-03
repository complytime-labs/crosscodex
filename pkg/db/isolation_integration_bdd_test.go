//go:build integration

package db_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

var _ = Describe("Transaction isolation from the context", func() {
	// isolationOf begins a tenant transaction on ctx and returns the
	// isolation level PostgreSQL reports for it. NewTenantPool runs
	// enter_tenant before any caller statement, so only the level set at
	// BEGIN can apply.
	isolationOf := func(ctx context.Context) string {
		setupTenant("iso-level", "Isolation Level Corp")
		// The Down/Up migration spec cannot drop graph_user while a tenant's
		// graph remains, so remove the tenant with the spec.
		DeferCleanup(func() { hardDeleteTenants("iso-level") })
		ctx, err := tenant.WithTenant(ctx, "iso-level")
		Expect(err).NotTo(HaveOccurred())
		tx, err := db.NewTenantPool(suPool).Begin(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = tx.Rollback() }()
		var level string
		Expect(tx.QueryRow(ctx, "SHOW transaction_isolation").Scan(&level)).To(Succeed())
		return level
	}

	It("starts a tenant transaction at the level the context carries", func() {
		Expect(isolationOf(db.WithIsolation(context.Background(), db.IsolationRepeatableRead))).To(Equal("repeatable read"))
	})

	It("starts at the server default without one", func() {
		Expect(isolationOf(context.Background())).To(Equal("read committed"))
	})

	It("refuses an unknown level before starting a transaction", func() {
		_, err := suPool.Begin(db.WithIsolation(context.Background(), "SOMETIMES"))
		Expect(err).To(MatchError(ContainSubstring(`unknown isolation level "SOMETIMES"`)))
	})
})

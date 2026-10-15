//go:build !integration

package db_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

var _ = Describe("tenant scope statement", func() {
	begin := func(newConn func(db.Pool) db.TenantConnection) []string {
		pool, execs := db.ExportNewRecordingPool()
		ctx, err := tenant.WithTenant(context.Background(), "acme")
		Expect(err).NotTo(HaveOccurred())
		_, err = newConn(pool).Begin(ctx)
		Expect(err).NotTo(HaveOccurred())
		return execs()
	}

	It("NewTenantPool enters the tenant through enter_tenant, which refuses deleted tenants", func() {
		Expect(begin(db.NewTenantPool)).To(Equal([]string{"SELECT public.enter_tenant($1)[acme]"}))
	})

	It("NewRetentionTenantPool sets the tenant directly so deleted tenants stay reachable", func() {
		Expect(begin(db.NewRetentionTenantPool)).To(Equal([]string{"SELECT set_config('app.current_tenant', $1, true)[acme]"}))
	})

	It("NewRetentionTenantPool still requires a tenant in the context", func() {
		_, err := db.NewRetentionTenantPool(nil).Begin(context.Background())
		Expect(err).To(MatchError(db.ErrTenantRequired))
	})
})

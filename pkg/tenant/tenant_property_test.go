package tenant_test

import (
	"context"
	"regexp"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// Suite bootstrap lives in tenant_bdd_test.go (TestTenantBDD).
// This file only registers Describe nodes; Ginkgo collects them automatically.

var tenantIDRegex = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}[a-z0-9]$`)

var _ = Describe("Property Specifications", Ordered, func() {
	Context("ValidateTenantID", func() {
		It("accepts only IDs matching the canonical regex", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				id := rapid.String().Draw(t, "id")
				err := tenant.ValidateTenantID(id)
				if err == nil {
					Expect(tenantIDRegex.MatchString(id)).To(BeTrue(),
						"ValidateTenantID accepted %q which does not match regex", id)
				}
			})
		})

		It("rejects all IDs shorter than 3 characters", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				id := rapid.StringN(0, 2, 2).Draw(t, "short-id")
				err := tenant.ValidateTenantID(id)
				Expect(err).To(HaveOccurred(),
					"ValidateTenantID accepted short ID %q", id)
			})
		})

		It("rejects all IDs longer than 64 characters", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				id := rapid.StringN(65, 200, -1).Draw(t, "long-id")
				err := tenant.ValidateTenantID(id)
				Expect(err).To(HaveOccurred(),
					"ValidateTenantID accepted long ID %q (len=%d)", id, len(id))
			})
		})
	})

	Context("WithTenant / FromContext roundtrip", func() {
		It("round-trips valid tenant IDs through context", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				id := rapid.StringMatching(`[a-z][a-z0-9-]{1,62}[a-z0-9]`).Draw(t, "tenant-id")
				ctx := context.Background()
				ctx, err := tenant.WithTenant(ctx, id)
				Expect(err).NotTo(HaveOccurred(),
					"WithTenant failed for valid ID %q", id)
				got, err := tenant.FromContext(ctx)
				Expect(err).NotTo(HaveOccurred(),
					"FromContext failed for ID %q", id)
				Expect(got).To(Equal(id),
					"roundtrip mismatch: put %q, got %q", id, got)
			})
		})
	})
})

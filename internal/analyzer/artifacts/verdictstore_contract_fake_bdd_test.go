//go:build !integration

package artifacts_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/analyzer/artifacts"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

var _ = Describe("fakeVerdictStore contract", func() {
	verdictStoreContract(func() verdictStoreContractEnv {
		const id = "contract-fake"
		s := newFakeVerdictStore()
		ctx, err := tenant.WithTenant(context.Background(), id)
		Expect(err).NotTo(HaveOccurred())
		return verdictStoreContractEnv{store: s, ctx: ctx, tenantID: id,
			setHuman: func(key string, status artifacts.VerdictStatus) { s.setHuman(id, key, status) },
			row: func(key string) contractRow {
				r := s.row(id, key)
				Expect(r).NotTo(BeNil())
				return contractRow{status: r.status,
					graphApplied: r.graphApplied, attempts: r.attempts, hasDissent: r.dissent != nil}
			}}
	})
})

//go:build integration

package db_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/db"
)

// The suite entry point lives in db_integration_bdd_test.go.
var _ = Describe("AdvisoryLocker", func() {
	ctx := context.Background()

	BeforeEach(func() {
		if suPool == nil {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}
	})

	It("excludes a second session until released, as the unprivileged app_user", func() {
		first := db.NewAdvisoryLocker(appUserDSN(), "crosscodex:it-lock")
		second := db.NewAdvisoryLocker(appUserDSN(), "crosscodex:it-lock")

		release, err := first.TryAcquire(ctx)
		Expect(err).NotTo(HaveOccurred())

		_, err = second.TryAcquire(ctx)
		Expect(err).To(MatchError(db.ErrLockHeld))
		Expect(err.Error()).To(ContainSubstring("retry later"))

		Expect(release(ctx)).To(Succeed())

		release2, err := second.TryAcquire(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(release2(ctx)).To(Succeed())
	})

	It("does not block a lock with a different name", func() {
		a, err := db.NewAdvisoryLocker(appUserDSN(), "crosscodex:it-a").TryAcquire(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer func() { Expect(a(ctx)).To(Succeed()) }()
		b, err := db.NewAdvisoryLocker(appUserDSN(), "crosscodex:it-b").TryAcquire(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(b(ctx)).To(Succeed())
	})
})

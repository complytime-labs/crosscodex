package db_test

import (
	"context"
	"hash/fnv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/db"
)

var _ = Describe("AdvisoryLockKey", func() {
	It("is the 64-bit FNV-1a hash of the name", func() {
		h := fnv.New64a()
		_, _ = h.Write([]byte(db.BackupRetentionLockName))
		Expect(uint64(db.AdvisoryLockKey(db.BackupRetentionLockName))).To(Equal(h.Sum64()))
	})

	It("differs for different names", func() {
		Expect(db.AdvisoryLockKey("a")).NotTo(Equal(db.AdvisoryLockKey("b")))
	})
})

var _ = Describe("AdvisoryLocker.TryAcquire", func() {
	It("fails with the lock name and without the password when the database is unreachable", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		l := db.NewAdvisoryLocker("postgres://u:topsecret@127.0.0.1:1/x?sslmode=disable&connect_timeout=1", "crosscodex:test")
		release, err := l.TryAcquire(ctx)
		Expect(release).To(BeNil())
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("crosscodex:test"))
		Expect(err.Error()).NotTo(ContainSubstring("topsecret"))
	})
})

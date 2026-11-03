package db_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/db"
)

var _ = Describe("WithIsolation", func() {
	It("carries the isolation level the next Begin starts at", func() {
		ctx := db.WithIsolation(context.Background(), db.IsolationRepeatableRead)
		level, ok := db.IsolationFromContext(ctx)
		Expect(ok).To(BeTrue())
		Expect(level).To(Equal(db.IsolationRepeatableRead))
	})

	It("carries none by default, so Begin keeps the server default", func() {
		_, ok := db.IsolationFromContext(context.Background())
		Expect(ok).To(BeFalse())
	})
})

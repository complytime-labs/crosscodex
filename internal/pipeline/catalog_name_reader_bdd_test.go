package pipeline_test

import (
	"context"
	"database/sql"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/pipeline"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
)

var _ = Describe("PGCatalogNameReader", func() {
	It("refuses an invalid tenant before any transaction starts", func() {
		conn := &dbtest.Conn{}
		_, err := pipeline.NewPGCatalogNameReader(conn).CatalogName(context.Background(), "", "cat-1")
		Expect(err).To(HaveOccurred())
		Expect(conn.Begins()).To(BeZero())
	})

	It("reads the catalog's name, scoped to the tenant", func() {
		var gotQuery string
		var gotArgs []any
		tx := &dbtest.Tx{QueryRowFunc: func(_ context.Context, query string, args ...any) db.Row {
			gotQuery, gotArgs = query, args
			return dbtest.Row(nil, "NIST SP 800-53")
		}}
		name, err := pipeline.NewPGCatalogNameReader(&dbtest.Conn{Tx: tx}).CatalogName(context.Background(), "tenant-1", "cat-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(name).To(Equal("NIST SP 800-53"))
		Expect(gotQuery).To(ContainSubstring("FROM catalogs WHERE tenant_id = $1 AND catalog_id = $2"))
		Expect(gotArgs).To(Equal([]any{"tenant-1", "cat-1"}))
		Expect(tx.Commits()).To(Equal(1))
	})

	It("reports a catalog the tenant does not have as ErrCatalogNotFound, naming it", func() {
		tx := &dbtest.Tx{QueryRowFunc: func(context.Context, string, ...any) db.Row { return dbtest.Row(sql.ErrNoRows) }}
		_, err := pipeline.NewPGCatalogNameReader(&dbtest.Conn{Tx: tx}).CatalogName(context.Background(), "tenant-1", "cat-gone")
		Expect(err).To(MatchError(pipeline.ErrCatalogNotFound))
		Expect(err.Error()).To(ContainSubstring("cat-gone"))
	})

	It("wraps any other read error", func() {
		boom := errors.New("connection reset")
		tx := &dbtest.Tx{QueryRowFunc: func(context.Context, string, ...any) db.Row { return dbtest.Row(boom) }}
		_, err := pipeline.NewPGCatalogNameReader(&dbtest.Conn{Tx: tx}).CatalogName(context.Background(), "tenant-1", "cat-1")
		Expect(err).To(MatchError(boom))
		Expect(err).NotTo(MatchError(pipeline.ErrCatalogNotFound))
	})
})

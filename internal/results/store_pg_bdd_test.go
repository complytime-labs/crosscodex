package results_test

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/results"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
)

var _ = Describe("attachCleaning", func() {
	errDriver := errors.New("driver: connection reset")

	DescribeTable("fails ListMappings' page and attaches no cleaning state",
		func(rows func() *dbtest.FakeRows, queryErr error, want string) {
			var rs *dbtest.FakeRows
			tx := &dbtest.Tx{QueryFunc: func(context.Context, string, ...any) (db.Rows, error) {
				if queryErr != nil {
					return nil, queryErr
				}
				rs = rows()
				return rs, nil
			}}
			mappings := []results.Mapping{{JobID: "j1", SourceID: "a1", TargetID: "b1"}}

			err := results.AttachCleaning(context.Background(), tx, "tenant-a", mappings)
			Expect(err).To(MatchError(errDriver))
			Expect(err).To(MatchError(ContainSubstring(want)))
			Expect(mappings[0].Cleaning).To(BeNil())
			if rs != nil {
				Expect(rs.Closed()).To(BeTrue(), "the cleaning rows are closed")
			}
		},
		Entry("when the query fails", nil, errDriver, "list mappings: query cleaning states"),
		Entry("when a row does not scan", func() *dbtest.FakeRows {
			rs := dbtest.Rows(nil, []any{"j1", "a1", "b1", "retired", "finding", "f1", nil, false})
			rs.ScanErr = errDriver
			return rs
		}, nil, "list mappings: scan cleaning state"),
		Entry("when iteration fails", func() *dbtest.FakeRows {
			return dbtest.Rows(errDriver)
		}, nil, "list mappings: read cleaning states"),
	)
})

package graph_test

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/complytime-labs/crosscodex/internal/graph"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

var _ = Describe("PGControlsReader", func() {
	const catalogID = "0123456789abcdef"

	controlRows := [][]any{
		{catalogID + "/ac", catalogID, "ac", "Access Control", "", "family", ""},
		{catalogID + "/ac-1", catalogID, "ac-1", "Policy", "Develop a policy.", "SP800-53", "ac"},
	}

	var (
		ctx       context.Context
		rows      *dbtest.FakeRows
		queryErr  error
		gotArgs   []any
		tx        *dbtest.Tx
		beginErr  error
		conn      *dbtest.Conn
		beginSeen bool
	)

	BeforeEach(func() {
		var err error
		ctx, err = tenant.WithTenant(context.Background(), "tenant-a")
		Expect(err).NotTo(HaveOccurred())
		rows = dbtest.Rows(nil, controlRows...)
		queryErr = nil
		gotArgs = nil
		beginErr = nil
		beginSeen = false
		tx = &dbtest.Tx{
			QueryFunc: func(_ context.Context, _ string, args ...any) (db.Rows, error) {
				gotArgs = args
				if queryErr != nil {
					return nil, queryErr
				}
				return rows, nil
			},
		}
		conn = &dbtest.Conn{BeginFunc: func(context.Context) (db.Transaction, error) {
			beginSeen = true
			if beginErr != nil {
				return nil, beginErr
			}
			return tx, nil
		}}
	})

	It("lists the catalog's controls inside a tenant transaction, bound to the ctx tenant", func() {
		got, err := graph.NewPGControlsReader(conn).ListCatalogControls(ctx, catalogID)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal([]graph.CatalogControl{
			{ControlID: catalogID + "/ac", CatalogID: catalogID, Identifier: "ac", Title: "Access Control", Class: "family"},
			{ControlID: catalogID + "/ac-1", CatalogID: catalogID, Identifier: "ac-1", Title: "Policy",
				Statement: "Develop a policy.", Class: "SP800-53", ParentID: "ac"},
		}))
		Expect(gotArgs).To(Equal([]any{catalogID, "tenant-a"}), "query must bind catalog_id and the ctx tenant_id")
		Expect(rows.Closed()).To(BeTrue())
		Expect(tx.Commits()).To(Equal(1))
	})

	It("refuses a ctx without a tenant before touching the connection", func() {
		_, err := graph.NewPGControlsReader(conn).ListCatalogControls(context.Background(), catalogID)
		Expect(errors.Is(err, tenant.ErrNoTenant)).To(BeTrue())
		Expect(beginSeen).To(BeFalse())
	})

	DescribeTable("wraps database failures with the catalog ID and rolls back",
		func(setup func(), want error, rollbacks int) {
			setup()
			_, err := graph.NewPGControlsReader(conn).ListCatalogControls(ctx, catalogID)
			Expect(errors.Is(err, want)).To(BeTrue(), "got %v", err)
			Expect(err.Error()).To(ContainSubstring(catalogID))
			Expect(tx.Commits()).To(Equal(0))
			Expect(tx.Rollbacks()).To(Equal(rollbacks))
		},
		Entry("begin", func() { beginErr = db.ErrTenantRequired }, db.ErrTenantRequired, 0),
		Entry("query", func() { queryErr = errQueryFailed }, errQueryFailed, 1),
		Entry("scan", func() { rows.ScanErr = errQueryFailed }, errQueryFailed, 1),
		Entry("iteration", func() { rows = dbtest.Rows(errQueryFailed, controlRows...) }, errQueryFailed, 1),
	)

	Context("with telemetry", func() {
		var tp *telemetrytest.TestProvider

		BeforeEach(func() {
			var err error
			tp, err = telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = tp.Shutdown(context.Background()) })
		})

		newReader := func() *graph.PGControlsReader {
			return graph.NewPGControlsReader(conn,
				graph.WithPGControlsReaderTelemetry(tp.TracerProvider(), tp.MeterProvider()))
		}

		listPoints := func(status string) int64 {
			m := telemetrytest.FindMetric(tp.GetMetrics(), "graph.controls.list.total")
			if m == nil {
				return 0
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			Expect(ok).To(BeTrue())
			for _, dp := range sum.DataPoints {
				if s, _ := dp.Attributes.Value("status"); s.AsString() == status {
					return dp.Value
				}
			}
			return 0
		}

		durationCount := func() uint64 {
			m := telemetrytest.FindMetric(tp.GetMetrics(), "graph.controls.list.duration_ms")
			Expect(m).NotTo(BeNil())
			n, err := telemetrytest.Float64HistogramCount(m)
			Expect(err).NotTo(HaveOccurred())
			return n
		}

		It("records an ok span with tenant, catalog and row count, and an ok metric", func() {
			_, err := newReader().ListCatalogControls(ctx, catalogID)
			Expect(err).NotTo(HaveOccurred())

			span := telemetrytest.FindSpan(tp.GetSpans(), "graph.controls.list")
			Expect(span).NotTo(BeNil())
			Expect(span.Status().Code).To(Equal(codes.Ok))
			v, ok := telemetrytest.SpanAttribute(span, "tenant.id")
			Expect(ok).To(BeTrue())
			Expect(v.AsString()).To(Equal("tenant-a"))
			v, ok = telemetrytest.SpanAttribute(span, "catalog.id")
			Expect(ok).To(BeTrue())
			Expect(v.AsString()).To(Equal(catalogID))
			v, ok = telemetrytest.SpanAttribute(span, "controls.count")
			Expect(ok).To(BeTrue())
			Expect(v.AsInt64()).To(Equal(int64(2)))

			Expect(listPoints("ok")).To(Equal(int64(1)))
			Expect(listPoints("error")).To(BeZero())
			Expect(durationCount()).To(Equal(uint64(1)))
		})

		DescribeTable("records an error span and an error metric",
			func(setup func(), listCtx func() context.Context) {
				setup()
				_, err := newReader().ListCatalogControls(listCtx(), catalogID)
				Expect(err).To(HaveOccurred())

				span := telemetrytest.FindSpan(tp.GetSpans(), "graph.controls.list")
				Expect(span).NotTo(BeNil())
				Expect(span.Status().Code).To(Equal(codes.Error))
				_, hasCount := telemetrytest.SpanAttribute(span, "controls.count")
				Expect(hasCount).To(BeFalse())

				Expect(listPoints("error")).To(Equal(int64(1)))
				Expect(listPoints("ok")).To(BeZero())
				Expect(durationCount()).To(Equal(uint64(1)))
			},
			Entry("no tenant", func() {}, context.Background),
			Entry("query", func() { queryErr = errQueryFailed }, func() context.Context { return ctx }),
			Entry("scan", func() { rows.ScanErr = errQueryFailed }, func() context.Context { return ctx }),
		)
	})
})

var errQueryFailed = errors.New("connection reset by peer")

//go:build !integration

package db_test

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"regexp"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/db/migrations"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

var _ = Describe("TenantAdmin", func() {
	ctx := context.Background()
	var (
		tp    *telemetrytest.TestProvider
		admin *db.TenantAdmin
		calls func() int
	)

	BeforeEach(func() {
		var err error
		tp, err = telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = tp.Shutdown(context.Background()) })
		var pool db.Pool
		pool, calls = db.ExportNewFailingPool()
		admin, err = db.ExportNewTenantAdmin(pool, tp.TracerProvider().Tracer("test"), tp.MeterProvider().Meter("test"))
		Expect(err).NotTo(HaveOccurred())
	})

	DescribeTable("rejects bad input with an actionable message before any query",
		func(call func(*db.TenantAdmin) error, want string) {
			err := call(admin)
			Expect(err).To(MatchError(ContainSubstring(want)))
			Expect(calls()).To(BeZero())
		},
		Entry("Provision: malformed ID",
			func(a *db.TenantAdmin) error { _, err := a.Provision(ctx, "Bad_ID", "Bad"); return err },
			"Choose an ID that matches"),
		Entry("Provision: blank display name",
			func(a *db.TenantAdmin) error { _, err := a.Provision(ctx, "acme", " \t "); return err },
			"display name is empty"),
		Entry("Provision: control character in display name",
			func(a *db.TenantAdmin) error { _, err := a.Provision(ctx, "acme", "Acme\x1b[2J"); return err },
			"control character"),
		Entry("SetStatus: malformed ID",
			func(a *db.TenantAdmin) error {
				_, err := a.SetStatus(ctx, "-acme", db.TenantStatusSuspended)
				return err
			},
			"Choose an ID that matches"),
		Entry("SetStatus: unknown status",
			func(a *db.TenantAdmin) error { _, err := a.SetStatus(ctx, "acme", "deleted"); return err },
			`must be "active" or "suspended"`),
		Entry("Get: malformed ID",
			func(a *db.TenantAdmin) error { _, err := a.Get(ctx, "ACME"); return err },
			"Choose an ID that matches"),
		Entry("Import: empty list",
			func(a *db.TenantAdmin) error { _, err := a.Import(ctx, nil); return err },
			"the list is empty"),
		Entry("Import: malformed second ID",
			func(a *db.TenantAdmin) error {
				_, err := a.Import(ctx, []db.TenantSpec{{ID: "acme", DisplayName: "Acme"}, {ID: "x", DisplayName: "X"}})
				return err
			},
			"tenant 2:"),
		Entry("Import: duplicate IDs",
			func(a *db.TenantAdmin) error {
				_, err := a.Import(ctx, []db.TenantSpec{
					{ID: "acme", DisplayName: "Acme"}, {ID: "beta", DisplayName: "Beta"}, {ID: "acme", DisplayName: "Acme 2"},
				})
				return err
			},
			`tenants 1 and 3 both have ID "acme"`),
	)

	It("wraps tenant.ErrInvalidTenant for a malformed ID", func() {
		_, err := admin.Provision(ctx, "Bad_ID", "Bad")
		Expect(err).To(MatchError(tenant.ErrInvalidTenant))
	})

	It("wraps a database error from a valid call with the operation and tenant", func() {
		_, err := admin.Provision(ctx, "acme", "Acme")
		Expect(err).To(MatchError(db.ErrExportPoolUsed))
		Expect(err.Error()).To(ContainSubstring(`provision tenant "acme"`))
		Expect(calls()).To(Equal(1))
	})

	It("records a span with the tenant, an operations count and a duration", func() {
		_, _ = admin.Provision(ctx, "acme", "Acme")

		span := telemetrytest.FindSpan(tp.GetSpans(), "db.TenantAdmin.provision")
		Expect(span).NotTo(BeNil())
		v, ok := telemetrytest.SpanAttribute(span, "tenant.id")
		Expect(ok).To(BeTrue())
		Expect(v.AsString()).To(Equal("acme"))

		metrics := tp.GetMetrics()
		ops := telemetrytest.FindMetric(metrics, "db.tenant_admin.operations.total")
		Expect(ops).NotTo(BeNil())
		n, err := telemetrytest.CounterValue(ops)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(int64(1)))
		dur := telemetrytest.FindMetric(metrics, "db.tenant_admin.duration_ms")
		Expect(dur).NotTo(BeNil())
		count, err := telemetrytest.Float64HistogramCount(dur)
		Expect(err).NotTo(HaveOccurred())
		Expect(count).To(Equal(uint64(1)))
	})

	It("keeps a rejected ID off the span", func() {
		_, _ = admin.Provision(ctx, "Bad\x1b_ID", "Bad")

		span := telemetrytest.FindSpan(tp.GetSpans(), "db.TenantAdmin.provision")
		Expect(span).NotTo(BeNil())
		_, ok := telemetrytest.SpanAttribute(span, "tenant.id")
		Expect(ok).To(BeFalse())
	})

	Describe("transaction and not-found paths", func() {
		newAdmin := func(pool db.Pool) *db.TenantAdmin {
			a, err := db.ExportNewTenantAdmin(pool, tp.TracerProvider().Tracer("test"), tp.MeterProvider().Meter("test"))
			Expect(err).NotTo(HaveOccurred())
			return a
		}
		two := []db.TenantSpec{{ID: "acme", DisplayName: "Acme"}, {ID: "beta", DisplayName: "Beta"}}

		It("Import rolls back once, never commits, and reports the failing tenant", func() {
			cause := errors.New("provision boom")
			pool, counts := db.ExportNewScriptedTxPool([]error{nil, cause}, nil, nil)

			created, err := newAdmin(pool).Import(ctx, two)

			Expect(err).To(MatchError(cause))
			Expect(err.Error()).To(ContainSubstring("tenant 2 of 2"))
			Expect(err.Error()).To(ContainSubstring("Nothing was imported"))
			Expect(created).To(BeNil())
			commits, rollbacks := counts()
			Expect(commits).To(BeZero())
			Expect(rollbacks).To(Equal(1))
		})

		It("Import joins a rollback failure to the provisioning failure", func() {
			cause := errors.New("provision boom")
			rollbackErr := errors.New("rollback boom")
			pool, counts := db.ExportNewScriptedTxPool([]error{cause}, nil, rollbackErr)

			created, err := newAdmin(pool).Import(ctx, two)

			Expect(err).To(MatchError(cause))
			Expect(err).To(MatchError(rollbackErr))
			Expect(err.Error()).To(ContainSubstring("tenant 1 of 2"))
			Expect(created).To(BeNil())
			commits, rollbacks := counts()
			Expect(commits).To(BeZero())
			Expect(rollbacks).To(Equal(1))
		})

		It("Import wraps a Begin failure and provisions nothing", func() {
			created, err := admin.Import(ctx, two)

			Expect(err).To(MatchError(db.ErrExportPoolUsed))
			Expect(err.Error()).To(HavePrefix("import tenants: "))
			Expect(created).To(BeNil())
			Expect(calls()).To(Equal(1))
		})

		It("Import wraps a commit failure", func() {
			cause := errors.New("commit boom")
			pool, counts := db.ExportNewScriptedTxPool(nil, cause, nil)

			created, err := newAdmin(pool).Import(ctx, two)

			Expect(err).To(MatchError(cause))
			Expect(err.Error()).To(ContainSubstring("import tenants: commit"))
			Expect(created).To(BeNil())
			commits, _ := counts()
			Expect(commits).To(Equal(1))
		})

		It("SetStatus maps SQLSTATE P0002 to ErrTenantNotFound and keeps the cause", func() {
			cause := db.ExportSQLStateError("P0002")
			_, err := newAdmin(db.ExportNewScriptedRowPool(cause)).SetStatus(ctx, "acme", db.TenantStatusSuspended)

			Expect(err).To(MatchError(db.ErrTenantNotFound))
			Expect(err).To(MatchError(cause))
		})

		It("Get maps no rows to ErrTenantNotFound with the create command", func() {
			_, err := newAdmin(db.ExportNewScriptedRowPool(sql.ErrNoRows)).Get(ctx, "acme")

			Expect(err).To(MatchError(db.ErrTenantNotFound))
			Expect(err.Error()).To(ContainSubstring("crosscodexd admin tenant create"))
		})
	})

	Describe("List", func() {
		It("wraps a query failure and returns no records", func() {
			records, err := admin.List(ctx)

			Expect(err).To(MatchError(db.ErrExportPoolUsed))
			Expect(err.Error()).To(HavePrefix("list tenants: "))
			Expect(records).To(BeNil())
			Expect(calls()).To(Equal(1))
		})

		DescribeTable("returns no records and closes the rows when reading fails",
			func(n int, scanErr, rowsErr, closeErr error, wantPrefix string) {
				pool, closes := db.ExportNewScriptedRowsPool(n, scanErr, rowsErr, closeErr)
				a, err := db.ExportNewTenantAdmin(pool, tp.TracerProvider().Tracer("test"), tp.MeterProvider().Meter("test"))
				Expect(err).NotTo(HaveOccurred())

				records, err := a.List(ctx)

				for _, want := range []error{scanErr, rowsErr, closeErr} {
					if want != nil {
						Expect(err).To(MatchError(want))
					}
				}
				Expect(err.Error()).To(HavePrefix(wantPrefix))
				Expect(records).To(BeNil())
				Expect(closes()).To(Equal(1))
			},
			Entry("Scan fails", 2, errors.New("scan boom"), nil, nil, "list tenants: read row: "),
			Entry("rows.Err after iterating", 2, nil, errors.New("rows boom"), nil, "list tenants: "),
			Entry("Close fails after every row was read", 2, nil, nil, errors.New("close boom"), "close boom"),
			Entry("Scan and Close both fail", 1, errors.New("scan boom"), nil, errors.New("close boom"), "list tenants: read row: "),
		)
	})

	It("uses exactly the statuses migration 007's CHECK allows", func() {
		sqlText, err := fs.ReadFile(migrations.FS, "007_tenant_admin.up.sql")
		Expect(err).NotTo(HaveOccurred())
		m := regexp.MustCompile(`(?i)CHECK\s*\(\s*status\s+IN\s*\(([^)]*)\)\s*\)`).FindSubmatch(sqlText)
		Expect(m).NotTo(BeNil(), "tenants_status_check not found in migration 007")
		var allowed []string
		for _, q := range regexp.MustCompile(`'([^']*)'`).FindAllSubmatch(m[1], -1) {
			allowed = append(allowed, string(q[1]))
		}
		Expect(allowed).To(ConsistOf(db.TenantStatusActive, db.TenantStatusSuspended))
	})
})

var _ = Describe("TenantActive", func() {
	It("rejects a malformed tenant ID before querying", func() {
		pool, calls := db.ExportNewFailingPool()
		_, err := db.TenantActive(context.Background(), pool, "BAD")
		Expect(err).To(MatchError(tenant.ErrInvalidTenant))
		Expect(calls()).To(BeZero())
	})

	It("wraps a query failure with the tenant", func() {
		pool, calls := db.ExportNewFailingPool()
		_, err := db.TenantActive(context.Background(), pool, "acme")
		Expect(err).To(MatchError(db.ErrExportPoolUsed))
		Expect(err.Error()).To(ContainSubstring(`check tenant "acme" status`))
		Expect(calls()).To(Equal(1))
	})
})

var _ = Describe("ValidateTenantSpecs", func() {
	It("accepts distinct valid specs", func() {
		Expect(db.ValidateTenantSpecs([]db.TenantSpec{{ID: "acme", DisplayName: "Acme"}, {ID: "beta", DisplayName: "Beta"}})).To(Succeed())
	})
})

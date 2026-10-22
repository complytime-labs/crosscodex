//go:build !integration

package db_test

import (
	"context"
	"errors"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"go.opentelemetry.io/otel/codes"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

var _ = Describe("ValidateReason", func() {
	It("accepts printable text up to 500 bytes", func() {
		Expect(db.ValidateReason("contract ended")).To(Succeed())
		Expect(db.ValidateReason(strings.Repeat("a", 500))).To(Succeed())
		Expect(db.ValidateReason("  padded is fine  ")).To(Succeed())
	})
	DescribeTable("refuses with an actionable message",
		func(reason, want string) { Expect(db.ValidateReason(reason)).To(MatchError(ContainSubstring(want))) },
		Entry("empty", "", "--reason is empty"),
		Entry("blank", "   ", "--reason is empty"),
		Entry("too long", strings.Repeat("a", 501), "at most 500 bytes"),
		Entry("control char", "a\x01b", "control, format or line-separator"),
		Entry("newline", "a\nb", "control, format or line-separator"),
		Entry("bidi override", "a\u202eb", "control, format or line-separator"),
		Entry("line separator", "a\u2028b", "control, format or line-separator"),
		Entry("invalid UTF-8", "a\xffb", "UTF-8"),
	)
})

var _ = Describe("ValidateActor", func() {
	It("accepts printable text up to 256 bytes", func() {
		Expect(db.ValidateActor("ops@example.invalid")).To(Succeed())
		Expect(db.ValidateActor(strings.Repeat("a", 256))).To(Succeed())
	})
	DescribeTable("refuses with an actionable message",
		func(actor, want string) { Expect(db.ValidateActor(actor)).To(MatchError(ContainSubstring(want))) },
		Entry("empty", "", "--by is empty"),
		Entry("blank", " \t", "--by is empty"),
		Entry("too long", strings.Repeat("a", 257), "at most 256 bytes"),
		Entry("control char", "ops\x1b[2J", "control, format or line-separator"),
		Entry("bidi override", "ops\u202e", "control, format or line-separator"),
		Entry("invalid UTF-8", "ops\xff", "UTF-8"),
		Entry("leading space", " ops", "leading or trailing spaces"),
		Entry("trailing space", "ops ", "leading or trailing spaces"),
	)
})

var _ = Describe("ValidateReviewerID", func() {
	DescribeTable("accepts",
		func(id string) { Expect(db.ValidateReviewerID(id)).To(Succeed()) },
		Entry("an email", "alice@example.com"),
		Entry("a name with spaces", "Alice Smith"),
		Entry("exactly 256 bytes", strings.Repeat("a", db.MaxReviewerBytes)),
	)
	DescribeTable("rejects with an actionable message",
		func(id, want string) {
			Expect(db.ValidateReviewerID(id)).To(MatchError(ContainSubstring(want)))
		},
		Entry("empty", "", "pass the ID of the person recording this verdict"),
		Entry("blank", "   ", "pass the ID of the person recording this verdict"),
		Entry("257 bytes", strings.Repeat("a", 257), "at most 256 bytes"),
		Entry("invalid UTF-8", "al\xffice", "valid UTF-8"),
		Entry("a newline", "alice\nbob", "control, format or line-separator characters"),
		Entry("an ANSI escape", "alice\x1b[31m", "control, format or line-separator characters"),
		Entry("a bidi override", "alice\u202e", "control, format or line-separator characters"),
		Entry("a line separator", "alice\u2028bob", "control, format or line-separator characters"),
		Entry("a paragraph separator", "alice\u2029bob", "control, format or line-separator characters"),
		Entry("a leading space", " alice", "leading or trailing spaces"),
		Entry("a trailing space", "alice ", "leading or trailing spaces"),
		Entry("a trailing no-break space", "alice\u00a0", "leading or trailing spaces"),
	)
})

var _ = Describe("ValidatePseudonym", func() {
	DescribeTable("accepts", func(p string) { Expect(db.ValidatePseudonym(p)).To(Succeed()) },
		Entry("tenant", "hidden-tenant-0123456789abcdef"),
		Entry("reviewer", "hidden-reviewer-0123456789abcdef"))
	DescribeTable("refuses", func(p string) {
		Expect(db.ValidatePseudonym(p)).To(MatchError(ContainSubstring("hidden-tenant-<16 hex>")))
	},
		Entry("empty", ""), Entry("uppercase hex", "hidden-tenant-0123456789ABCDEF"),
		Entry("short", "hidden-tenant-0123"), Entry("other kind", "hidden-job-0123456789abcdef"),
		Entry("suffix", "hidden-tenant-0123456789abcdef0"),
		Entry("trailing newline", "hidden-tenant-0123456789abcdef\n"),
		Entry("injection", "hidden-tenant-0123456789abcdef'; DROP TABLE tenants; --"))
})

var _ = Describe("IdentityRequest.Canonical", func() {
	It("is deterministic with fixed key order and UTC time", func() {
		at := time.Date(2026, 10, 14, 1, 2, 3, 0, time.FixedZone("x", 3600))
		r := db.IdentityRequest{Action: "delete", Kind: "tenant", TenantID: "acme", IdentityID: "acme",
			Actor: "ops", Reason: "gone", RequestedAt: at}
		b, err := r.Canonical()
		Expect(err).NotTo(HaveOccurred())
		Expect(string(b)).To(Equal(`{"action":"delete","kind":"tenant","tenant_id":"acme","identity_id":"acme","actor":"ops","reason":"gone","requested_at":"2026-10-14T00:02:03Z"}`))
		again, err := r.Canonical()
		Expect(err).NotTo(HaveOccurred())
		Expect(again).To(Equal(b))
	})

	It("refuses a zero RequestedAt, which would sign a meaningless time", func() {
		_, err := db.IdentityRequest{Action: "delete", Kind: "tenant", TenantID: "acme", IdentityID: "acme",
			Actor: "ops", Reason: "gone"}.Canonical()
		Expect(err).To(MatchError(ContainSubstring("requested_at is not set")))
	})

	It("keeps empty kind and tenant_id for a trace", func() {
		r := db.IdentityRequest{Action: "trace", IdentityID: "hidden-tenant-0123456789abcdef",
			Actor: "ops", Reason: "audit", RequestedAt: time.Date(2026, 10, 14, 0, 0, 0, 5, time.UTC)}
		b, err := r.Canonical()
		Expect(err).NotTo(HaveOccurred())
		Expect(string(b)).To(Equal(`{"action":"trace","kind":"","tenant_id":"","identity_id":"hidden-tenant-0123456789abcdef","actor":"ops","reason":"audit","requested_at":"2026-10-14T00:00:00.000000005Z"}`))
	})
})

var _ = Describe("ValidateTenantSpec", func() {
	It("refuses the reserved hidden- prefix with an actionable message", func() {
		err := db.ValidateTenantSpec(db.TenantSpec{ID: "hidden-acme", DisplayName: "Acme"})
		Expect(err).To(MatchError(tenant.ErrInvalidTenant))
		Expect(err.Error()).To(ContainSubstring(`"hidden-" are reserved`))
		Expect(err.Error()).To(ContainSubstring("choose another ID"))
	})

	It("accepts hidden as an ordinary word elsewhere in the ID", func() {
		Expect(db.ValidateTenantSpec(db.TenantSpec{ID: "not-hidden-acme", DisplayName: "Acme"})).To(Succeed())
		Expect(db.ValidateTenantSpec(db.TenantSpec{ID: "hiddenacme", DisplayName: "Acme"})).To(Succeed())
	})
})

var _ = Describe("TenantAdmin identity methods", func() {
	const (
		actor     = "ops@example.invalid"
		reason    = "customer offboarded"
		pseudonym = "hidden-tenant-0123456789abcdef"
	)
	ctx := context.Background()
	signed := db.SignedRequest{Request: []byte("{}"), Attestation: []byte("{}")}
	var (
		tp    *telemetrytest.TestProvider
		admin *db.TenantAdmin
		calls func() int
	)
	newAdmin := func(pool db.Pool) *db.TenantAdmin {
		a, err := db.ExportNewTenantAdmin(pool, tp.TracerProvider().Tracer("test"), tp.MeterProvider().Meter("test"))
		Expect(err).NotTo(HaveOccurred())
		return a
	}

	BeforeEach(func() {
		var err error
		tp, err = telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = tp.Shutdown(context.Background()) })
		var pool db.Pool
		pool, calls = db.ExportNewFailingPool()
		admin = newAdmin(pool)
	})

	// call runs one identity method with the given tenant, identity (reviewer
	// or pseudonym), actor and reason, discarding any non-error result.
	type call func(a *db.TenantAdmin, tenantID, identity, actor, reason string) error
	var (
		deleteTenant call = func(a *db.TenantAdmin, t, _, by, why string) error {
			return a.DeleteTenant(ctx, t, by, why, signed)
		}
		restoreTenant call = func(a *db.TenantAdmin, t, _, by, why string) error {
			return a.RestoreTenant(ctx, t, by, why, signed)
		}
		hideTenant call = func(a *db.TenantAdmin, t, _, by, why string) error {
			_, err := a.HideTenant(ctx, t, by, why, signed)
			return err
		}
		unhideTenant call = func(a *db.TenantAdmin, t, _, by, why string) error {
			return a.UnhideTenant(ctx, t, by, why, signed)
		}
		addReviewer call = func(a *db.TenantAdmin, t, r, by, why string) error {
			_, err := a.AddReviewer(ctx, t, r, by, why, signed)
			return err
		}
		deleteReviewer call = func(a *db.TenantAdmin, t, r, by, why string) error {
			return a.DeleteReviewer(ctx, t, r, by, why, signed)
		}
		restoreReviewer call = func(a *db.TenantAdmin, t, r, by, why string) error {
			return a.RestoreReviewer(ctx, t, r, by, why, signed)
		}
		hideReviewer call = func(a *db.TenantAdmin, t, r, by, why string) error {
			_, err := a.HideReviewer(ctx, t, r, by, why, signed)
			return err
		}
		unhideReviewer call = func(a *db.TenantAdmin, t, r, by, why string) error {
			return a.UnhideReviewer(ctx, t, r, by, why, signed)
		}
		trace call = func(a *db.TenantAdmin, _, p, by, why string) error {
			_, err := a.Trace(ctx, p, by, why, signed)
			return err
		}
	)

	DescribeTable("refuses bad input with an actionable message before any query",
		func(c call, tenantID, identity, by, why, want string) {
			err := c(admin, tenantID, identity, by, why)
			Expect(err).To(MatchError(ContainSubstring(want)))
			Expect(calls()).To(BeZero())
		},
		Entry("DeleteTenant: malformed ID", deleteTenant, "Bad_ID", "", actor, reason, "Choose an ID that matches"),
		Entry("RestoreTenant: empty reason", restoreTenant, "acme", "", actor, " ", "--reason is empty"),
		Entry("HideTenant: actor with control char", hideTenant, "acme", "", "ops\n", reason, "--by contains control"),
		Entry("AddReviewer: empty reviewer", addReviewer, "acme", "", actor, reason, "reviewer is empty"),
		Entry("DeleteReviewer: reviewer with escape", deleteReviewer, "acme", "alice\x1b[2J", actor, reason, "reviewer contains control"),
		Entry("RestoreReviewer: padded reviewer", restoreReviewer, "acme", "alice ", actor, reason, "reviewer has leading or trailing spaces"),
		Entry("HideReviewer: oversize reviewer", hideReviewer, "acme", strings.Repeat("a", 257), actor, reason, "at most 256 bytes"),
		Entry("UnhideReviewer: invalid UTF-8 reviewer", unhideReviewer, "acme", "al\xffice", actor, reason, "reviewer is not valid UTF-8"),
		Entry("UnhideTenant: empty actor", unhideTenant, "acme", "", "", reason, "--by is empty"),
		Entry("AddReviewer: malformed tenant", addReviewer, "-acme", "alice", actor, reason, "Choose an ID that matches"),
		Entry("DeleteReviewer: oversize reason", deleteReviewer, "acme", "alice", actor, strings.Repeat("a", 501), "at most 500 bytes"),
		Entry("RestoreReviewer: padded actor", restoreReviewer, "acme", "alice", " ops", reason, "leading or trailing spaces"),
		Entry("HideReviewer: bidi reason", hideReviewer, "acme", "alice", actor, "x\u202ey", "control, format or line-separator"),
		Entry("UnhideReviewer: malformed tenant", unhideReviewer, "ACME", "alice", actor, reason, "Choose an ID that matches"),
		Entry("Trace: malformed pseudonym", trace, "", "acme", actor, reason, "hidden-tenant-<16 hex>"),
		Entry("Trace: empty reason", trace, "", pseudonym, actor, "", "--reason is empty"),
	)

	It("ListReviewers refuses a malformed tenant ID before any query", func() {
		records, err := admin.ListReviewers(ctx, "Bad_ID")
		Expect(err).To(MatchError(tenant.ErrInvalidTenant))
		Expect(records).To(BeNil())
		Expect(calls()).To(BeZero())
	})

	DescribeTable("maps SQLSTATEs to sentinels, keeping the database's message",
		func(c call, identity, code string, want error, prefix string) {
			cause := db.ExportSQLStateError(code)
			err := c(newAdmin(db.ExportNewScriptedRowPool(cause)), "acme", identity, actor, reason)
			Expect(err).To(MatchError(want))
			Expect(err).To(MatchError(cause))
			Expect(err.Error()).To(HavePrefix(prefix))
		},
		Entry("DeleteTenant unknown", deleteTenant, "", "P0002", db.ErrTenantNotFound, `delete tenant "acme": `),
		Entry("RestoreTenant not deleted", restoreTenant, "", "55000", db.ErrIdentityState, `restore tenant "acme": `),
		Entry("HideTenant unknown", hideTenant, "", "P0002", db.ErrTenantNotFound, `hide tenant "acme": `),
		Entry("HideTenant active", hideTenant, "", "55000", db.ErrIdentityState, `hide tenant "acme": `),
		Entry("UnhideTenant not hidden", unhideTenant, "", "55000", db.ErrIdentityState, `unhide tenant "acme": `),
		Entry("AddReviewer unknown tenant", addReviewer, "alice", "P0002", db.ErrTenantNotFound, `add reviewer "alice" to tenant "acme": `),
		Entry("AddReviewer deleted reviewer", addReviewer, "alice", "55000", db.ErrIdentityState, `add reviewer "alice" to tenant "acme": `),
		Entry("DeleteReviewer unknown", deleteReviewer, "alice", "P0002", db.ErrReviewerNotFound, `delete reviewer "alice" of tenant "acme": `),
		Entry("RestoreReviewer unknown", restoreReviewer, "alice", "P0002", db.ErrReviewerNotFound, `restore reviewer "alice" of tenant "acme": `),
		Entry("RestoreReviewer hidden", restoreReviewer, "alice", "55000", db.ErrIdentityState, `restore reviewer "alice" of tenant "acme": `),
		Entry("HideReviewer unknown", hideReviewer, "alice", "P0002", db.ErrReviewerNotFound, `hide reviewer "alice" of tenant "acme": `),
		Entry("UnhideReviewer not hidden", unhideReviewer, "alice", "55000", db.ErrIdentityState, `unhide reviewer "alice" of tenant "acme": `),
		Entry("Trace unknown", trace, pseudonym, "P0002", db.ErrPseudonymNotFound, `trace pseudonym "`+pseudonym+`": `),
	)

	It("wraps any other database error without a sentinel", func() {
		cause := db.ExportSQLStateError("22023")
		err := deleteTenant(newAdmin(db.ExportNewScriptedRowPool(cause)), "acme", "", actor, reason)
		Expect(err).To(MatchError(cause))
		for _, sentinel := range []error{db.ErrTenantNotFound, db.ErrIdentityState, db.ErrReviewerNotFound, db.ErrPseudonymNotFound} {
			Expect(errors.Is(err, sentinel)).To(BeFalse(), "unexpected %v", sentinel)
		}
	})

	It("records a span with the tenant for a tenant action", func() {
		_ = deleteTenant(admin, "acme", "", actor, reason)
		span := telemetrytest.FindSpan(tp.GetSpans(), "db.TenantAdmin.delete_tenant")
		Expect(span).NotTo(BeNil())
		v, ok := telemetrytest.SpanAttribute(span, "tenant.id")
		Expect(ok).To(BeTrue())
		Expect(v.AsString()).To(Equal("acme"))
	})

	DescribeTable("puts only a coarse error description on the span, never the message",
		func(cause error, wantDescription string) {
			const reviewer = "alice@example.invalid"
			err := deleteReviewer(newAdmin(db.ExportNewScriptedRowPool(cause)), "acme", reviewer, actor, reason)
			Expect(err).To(MatchError(ContainSubstring(reviewer)), "the returned error keeps full detail")

			span := telemetrytest.FindSpan(tp.GetSpans(), "db.TenantAdmin.delete_reviewer")
			Expect(span).NotTo(BeNil())
			Expect(span.Status().Code).To(Equal(codes.Error))
			Expect(span.Status().Description).To(Equal(wantDescription))
			for _, ev := range span.Events() {
				Expect(ev.Name).NotTo(ContainSubstring(reviewer))
				for _, kv := range ev.Attributes {
					Expect(kv.Value.String()).NotTo(ContainSubstring(reviewer))
				}
			}
			for _, kv := range span.Attributes() {
				Expect(kv.Value.String()).NotTo(ContainSubstring(reviewer))
			}
		},
		Entry("a sentinel", db.ExportSQLStateError("P0002"), "reviewer_not_found"),
		Entry("a database error whose message names the reviewer",
			exportMessageError{state: "22023", msg: `reviewer "alice@example.invalid" is invalid`}, "sqlstate 22023"),
		Entry("any other error", errors.New(`connection lost while deleting "alice@example.invalid"`), "error"),
	)

	It("keeps the traced identity off the span", func() {
		_ = trace(admin, "", pseudonym, actor, reason)
		span := telemetrytest.FindSpan(tp.GetSpans(), "db.TenantAdmin.trace")
		Expect(span).NotTo(BeNil())
		_, ok := telemetrytest.SpanAttribute(span, "tenant.id")
		Expect(ok).To(BeFalse())
	})

	Describe("ListReviewers", func() {
		It("wraps a query failure and returns no records", func() {
			records, err := admin.ListReviewers(ctx, "acme")
			Expect(err).To(MatchError(db.ErrExportPoolUsed))
			Expect(err.Error()).To(HavePrefix(`list reviewers of tenant "acme": `))
			Expect(records).To(BeNil())
		})

		DescribeTable("returns no records and closes the rows when reading fails",
			func(scanErr, rowsErr, closeErr error, wantPrefix string) {
				pool, closes := db.ExportNewScriptedRowsPool(2, scanErr, rowsErr, closeErr)
				records, err := newAdmin(pool).ListReviewers(ctx, "acme")
				for _, want := range []error{scanErr, rowsErr, closeErr} {
					if want != nil {
						Expect(err).To(MatchError(want))
					}
				}
				Expect(err.Error()).To(HavePrefix(wantPrefix))
				Expect(records).To(BeNil())
				Expect(closes()).To(Equal(1))
			},
			Entry("Scan fails", errors.New("scan boom"), nil, nil, `list reviewers of tenant "acme": read row: `),
			Entry("rows.Err after iterating", nil, errors.New("rows boom"), nil, `list reviewers of tenant "acme": `),
			Entry("Close fails", nil, nil, errors.New("close boom"), "close boom"),
		)
	})
})

// exportMessageError is a database error carrying a SQLSTATE and a message,
// like a pgconn.PgError raised with a formatted MESSAGE.
type exportMessageError struct{ state, msg string }

func (e exportMessageError) Error() string    { return e.msg }
func (e exportMessageError) SQLState() string { return e.state }

var _ = Describe("TenantAdmin.ExportIdentityEvents", func() {
	ctx := context.Background()
	var tp *telemetrytest.TestProvider
	newAdmin := func(pool db.Pool) *db.TenantAdmin {
		a, err := db.ExportNewTenantAdmin(pool, tp.TracerProvider().Tracer("test"), tp.MeterProvider().Meter("test"))
		Expect(err).NotTo(HaveOccurred())
		return a
	}
	never := func(db.IdentityEvent) error {
		Fail("the callback must not run when the export fails before reading")
		return nil
	}

	BeforeEach(func() {
		var err error
		tp, err = telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = tp.Shutdown(context.Background()) })
	})

	It("wraps a failure to begin the snapshot and marks the span failed, without a tenant", func() {
		pool, _ := db.ExportNewFailingPool()
		err := newAdmin(pool).ExportIdentityEvents(ctx, never)
		Expect(err).To(MatchError(db.ErrExportPoolUsed))
		Expect(err.Error()).To(HavePrefix("export identity events: "))
		span := telemetrytest.FindSpan(tp.GetSpans(), "db.TenantAdmin.export_identity_events")
		Expect(span).NotTo(BeNil())
		Expect(span.Status().Code).To(Equal(codes.Error))
		_, ok := telemetrytest.SpanAttribute(span, "tenant.id")
		Expect(ok).To(BeFalse())
	})

	It("rolls back when the snapshot cannot be set", func() {
		pool, counts := db.ExportNewScriptedTxPool(nil, nil, nil)
		err := newAdmin(pool).ExportIdentityEvents(ctx, never)
		Expect(err).To(MatchError(db.ErrExportPoolUsed))
		Expect(err.Error()).To(HavePrefix("export identity events: "))
		commits, rollbacks := counts()
		Expect(commits).To(BeZero())
		Expect(rollbacks).To(Equal(1))
	})

	It("reads in one read-only repeatable-read snapshot and wraps a query failure", func() {
		pool, execs := db.ExportNewRecordingPool()
		err := newAdmin(pool).ExportIdentityEvents(ctx, never)
		Expect(err).To(MatchError(db.ErrExportPoolUsed))
		Expect(err.Error()).To(HavePrefix("export identity events: "))
		Expect(execs()).To(Equal([]string{"SET TRANSACTION ISOLATION LEVEL REPEATABLE READ, READ ONLY[]"}))
	})
})
